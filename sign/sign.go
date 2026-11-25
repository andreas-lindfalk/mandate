// Package sign signs and verifies deliveries. A delivery carries a
// timestamp and a signature over the timestamp and the body, so a receiver
// knows mandate sent it, that it was not changed, and that it is not a
// replay from long ago.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// Signature computes v1=<hex hmac-sha256(secret, timestamp + "." + body)>.
func Signature(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ErrInvalid is returned for a signature that does not verify.
var ErrInvalid = errors.New("invalid delivery signature")

// Verify checks a delivery's timestamp header and signature header against
// the body, as a receiver does.
func Verify(secret []byte, timestampHeader, signatureHeader string, body []byte, now time.Time) error {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrInvalid)
	}
	if delta := now.Unix() - timestamp; delta > int64(Tolerance.Seconds()) || delta < -int64(Tolerance.Seconds()) {
		return fmt.Errorf("%w: timestamp outside the tolerance", ErrInvalid)
	}
	want := Signature(secret, timestamp, body)
	if !hmac.Equal([]byte(want), []byte(strings.TrimSpace(signatureHeader))) {
		return ErrInvalid
	}
	return nil
}
