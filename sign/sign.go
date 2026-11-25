// Package sign signs and verifies deliveries, delivery contract v2.
//
// pgmandate signs with an Ed25519 key it keeps outside its database; a
// receiver holds only public keys, so neither a receiver nor a copy of the
// database can forge a delivery. The signature covers a timestamp and the
// exact body, and the body names its kind, its audience and its tenant, so
// a body signed for one receiver is not valid at another.
//
// A receiver checks, in this order: the signature (Verify), then the body's
// envelope (CheckEnvelope), then that it has not seen the delivery id, then
// notAfter. docs/delivery-v2.schema.json describes the body.
package sign

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Headers a delivery carries.
const (
	HeaderDeliveryId = "Mandate-Delivery-Id"
	HeaderTimestamp  = "Mandate-Timestamp"
	HeaderSignature  = "Mandate-Signature"
)

// Tolerance is how far a delivery's timestamp may be from the receiver's
// clock before it is refused.
const Tolerance = 5 * time.Minute

// Version is the version of the delivery body.
const Version = 2

// Kinds of signed bodies. A receiver of deliveries accepts KindDelivery
// only; KindCallback is reserved for calls to an agent's host.
const (
	KindDelivery = "delivery"
	KindCallback = "callback"
)

// signedPrefix starts the bytes that are signed, so a signature over a
// delivery can never be read as one over anything else.
const signedPrefix = "pgmandate-v2."

// ErrInvalid is returned for a delivery that does not verify.
var ErrInvalid = errors.New("invalid delivery signature")

// ErrUnknownKey is returned, wrapping ErrInvalid, for a signature by a key
// id the receiver does not trust: after a rotation, a reason to fetch the
// published set again.
var ErrUnknownKey = fmt.Errorf("%w: unknown key id", ErrInvalid)

// KeyId names a public key: the first 16 lower-case hex digits of its
// SHA-256.
func KeyId(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:8])
}

// Signer signs with one private key.
type Signer struct {
	private ed25519.PrivateKey
	kid     string
}

// NewSigner makes a signer from a 32-byte Ed25519 seed.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("sign: a key is a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	private := ed25519.NewKeyFromSeed(seed)
	return &Signer{private: private, kid: KeyId(private.Public().(ed25519.PublicKey))}, nil
}

// ParseSeed reads a seed in standard base64, as MANDATE_SIGNING_KEY holds it.
func ParseSeed(text string) ([]byte, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("sign: a key is a %d-byte seed in standard base64", ed25519.SeedSize)
	}
	return seed, nil
}

func (s *Signer) KeyId() string { return s.kid }

func (s *Signer) PublicKey() ed25519.PublicKey { return s.private.Public().(ed25519.PublicKey) }

// Sign returns the signature header for a body sent at the timestamp:
// v2 kid=<key id> sig=<standard base64 signature>.
func (s *Signer) Sign(timestamp int64, body []byte) string {
	sig := ed25519.Sign(s.private, signedBytes(strconv.FormatInt(timestamp, 10), body))
	return "v2 kid=" + s.kid + " sig=" + base64.StdEncoding.EncodeToString(sig)
}

func signedBytes(timestamp string, body []byte) []byte {
	out := make([]byte, 0, len(signedPrefix)+len(timestamp)+1+len(body))
	out = append(out, signedPrefix...)
	out = append(out, timestamp...)
	out = append(out, '.')
	return append(out, body...)
}

// KeySet is the public keys a receiver trusts, by key id.
type KeySet map[string]ed25519.PublicKey

// NewKeySet trusts the given public keys.
func NewKeySet(keys ...ed25519.PublicKey) KeySet {
	set := KeySet{}
	for _, key := range keys {
		set[KeyId(key)] = key
	}
	return set
}

// ParsePublicKey reads a public key in standard base64.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("sign: a public key is %d bytes in standard base64", ed25519.PublicKeySize)
	}
	return key, nil
}

// Published is the key set pgmandate serves at /.well-known/pgmandate-keys.
// A receiver that fetches it replaces what it had with what it gets, so a
// key dropped from the set is no longer trusted.
type Published struct {
	Keys []PublishedKey `json:"keys"`
}

type PublishedKey struct {
	Kid    string `json:"kid"`
	Alg    string `json:"alg"`
	Public string `json:"public"`
}

// Publish describes public keys as the well-known document does.
func Publish(keys ...ed25519.PublicKey) Published {
	out := Published{Keys: []PublishedKey{}}
	for _, key := range keys {
		out.Keys = append(out.Keys, PublishedKey{Kid: KeyId(key), Alg: "Ed25519", Public: base64.StdEncoding.EncodeToString(key)})
	}
	return out
}

// KeySet reads a published key set, refusing a key whose id does not match.
func (p Published) KeySet() (KeySet, error) {
	set := KeySet{}
	for _, k := range p.Keys {
		if k.Alg != "Ed25519" {
			return nil, fmt.Errorf("sign: key %s is %s, not Ed25519", k.Kid, k.Alg)
		}
		key, err := ParsePublicKey(k.Public)
		if err != nil {
			return nil, err
		}
		if KeyId(key) != k.Kid {
			return nil, fmt.Errorf("sign: key %s does not match its id", k.Kid)
		}
		set[k.Kid] = key
	}
	return set, nil
}

// Fetch reads the published key set at url, as a receiver does at start
// and again on a key id it does not know. Anything but a 200 with at least
// one key is an error, so a receiver never starts trusting nothing. Fetch
// it over https from an address the receiver is configured with.
func Fetch(ctx context.Context, client *http.Client, url string) (KeySet, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sign: fetching the published keys: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sign: fetching the published keys: %s answered %d", url, resp.StatusCode)
	}
	var published Published
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&published); err != nil {
		return nil, fmt.Errorf("sign: reading the published keys: %w", err)
	}
	set, err := published.KeySet()
	if err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("sign: %s publishes no keys", url)
	}
	return set, nil
}

// Verify checks a delivery's timestamp and signature headers against the
// raw body, before anything is parsed: the key id must be one the receiver
// trusts, the signature must hold, and the timestamp must be within
// Tolerance of now.
func Verify(keys KeySet, timestampHeader, signatureHeader string, body []byte, now time.Time) error {
	timestampText := strings.TrimSpace(timestampHeader)
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || strconv.FormatInt(timestamp, 10) != timestampText {
		return fmt.Errorf("%w: bad timestamp", ErrInvalid)
	}
	if delta := now.Unix() - timestamp; delta > int64(Tolerance.Seconds()) || delta < -int64(Tolerance.Seconds()) {
		return fmt.Errorf("%w: timestamp outside the tolerance", ErrInvalid)
	}
	fields := strings.Fields(signatureHeader)
	if len(fields) != 3 || fields[0] != "v2" || !strings.HasPrefix(fields[1], "kid=") || !strings.HasPrefix(fields[2], "sig=") {
		return fmt.Errorf("%w: the header is not v2 kid=… sig=…", ErrInvalid)
	}
	key, ok := keys[strings.TrimPrefix(fields[1], "kid=")]
	if !ok {
		return ErrUnknownKey
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fields[2], "sig="))
	if err != nil || !ed25519.Verify(key, signedBytes(timestampText, body), sig) {
		return ErrInvalid
	}
	return nil
}

// Envelope is what a receiver checks of a verified body before it acts.
type Envelope struct {
	Version    int       `json:"version"`
	Kind       string    `json:"kind"`
	Audience   string    `json:"audience"`
	TenantId   string    `json:"tenantId"`
	DeliveryId string    `json:"deliveryId"`
	NotAfter   time.Time `json:"notAfter"`
}

// ErrEnvelope is returned for a verified body that is not for this receiver.
var ErrEnvelope = errors.New("the delivery is not for this receiver")

// CheckEnvelope reads a verified body's envelope and checks it is a version
// 2 delivery for this receiver's audience and tenant, which the receiver is
// configured with and never takes from the request. The delivery id must
// equal the header's.
func CheckEnvelope(body []byte, deliveryIdHeader, audience, tenantId string) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrEnvelope, err)
	}
	switch {
	case e.Version != Version:
		return Envelope{}, fmt.Errorf("%w: version %d", ErrEnvelope, e.Version)
	case e.Kind != KindDelivery:
		return Envelope{}, fmt.Errorf("%w: kind %q", ErrEnvelope, e.Kind)
	case e.Audience != audience || audience == "":
		return Envelope{}, fmt.Errorf("%w: another audience", ErrEnvelope)
	case e.TenantId != tenantId || tenantId == "":
		return Envelope{}, fmt.Errorf("%w: another tenant", ErrEnvelope)
	case e.DeliveryId == "" || e.DeliveryId != strings.TrimSpace(deliveryIdHeader):
		return Envelope{}, fmt.Errorf("%w: the delivery id differs from the header's", ErrEnvelope)
	case e.NotAfter.IsZero():
		return Envelope{}, fmt.Errorf("%w: no notAfter", ErrEnvelope)
	}
	return e, nil
}
