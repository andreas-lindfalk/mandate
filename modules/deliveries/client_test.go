package deliveries

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/pkg/events"
)

// A connector may not live on this machine or a private network unless the
// operator said so, and a redirect is never followed.
func TestClientRefusesPrivateAddressesAndRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := newClient(false).Post(target.URL, "application/json", nil) //nolint:noctx,bodyclose // a test; the call is refused, so there is no body
	require.ErrorIs(t, err, errBlocked, "loopback is refused by default")
	require.Zero(t, hits)
	require.Equal(t, "blocked: the receiver's address is private or local", describe(err, 0))

	resp, err := newClient(true).Post(target.URL, "application/json", nil) //nolint:noctx // a test
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, hits)

	resp, err = newClient(true).Post(redirector.URL, "application/json", nil) //nolint:noctx // a test
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode, "the redirect is the answer, not followed")
	require.Equal(t, 1, hits)
}

func TestBlockedAddresses(t *testing.T) {
	for address, blocked := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.9": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.1.2.3": true, "198.18.0.1": true, "240.0.0.1": true, "192.0.0.8": true,
		"::1": true, "fc00::1": true, "fd12:3456::1": true, "fe80::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"64:ff9b::a00:1": true, "2002:a00:1::1": true, "::": true, "fec0::1": true, "64:ff9b:1::a00:1": true, "2001:0:4136:e378::1": true,
		"93.184.216.34": false, "8.8.8.8": false, "2606:2800:220:1::1": false, "::ffff:8.8.8.8": false,
	} {
		require.Equal(t, blocked, blockedAddr(netip.MustParseAddr(address)), address)
	}
}

func TestDescribeNeverLeaksTheAddress(t *testing.T) {
	require.Equal(t, "the receiver answered 503 without an outcome", describe(nil, 503))
	require.Equal(t, "the receiver could not be reached", describe(errors.New(`Post "http://user:secret@10.0.0.5:6379/x": dial tcp 10.0.0.5:6379: connect: connection refused`), 0))
}

// When the attempt cannot be recorded, the job's error still says what
// went wrong in words, never in the client's text, which names the address
// and any token in its query.
func TestAFailedRecordNeverLeaksTheAddress(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := closed.URL + "/hooks?token=s3cr3t"
	closed.Close()
	pool, err := pgxpool.New(context.Background(), "postgres://127.0.0.1:1/none")
	require.NoError(t, err)
	pool.Close()
	s := &Service{pool: pool, client: newClient(true), allowPrivate: true}

	r := s.post(context.Background(), address, uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.ErrorContains(t, r.err, "s3cr3t", "the client's own error names the address")
	err = s.closed(context.Background(), Delivery{Id: uuid.New()}, r, false, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "the receiver could not be reached")
	require.ErrorContains(t, err, "closed pool", "the database's error goes on")
	require.NotContains(t, err.Error(), "s3cr3t")
	require.NotContains(t, err.Error(), closed.URL)
}

func TestPlainHttpToAPublicAddressIsRefusedUnsent(t *testing.T) {
	strict := &Service{client: newClient(false)}
	r := strict.post(context.Background(), "http://connector.example/hooks", uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.ErrorIs(t, r.err, errPlain)
	require.False(t, r.sent, "refused before anything was written")
	require.Equal(t, "blocked: "+errPlain.Error(), describe(r.err, 0))
	r = strict.post(context.Background(), "HTTPS://127.0.0.1:1/hooks", uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.ErrorIs(t, r.err, errBlocked, "https gets past the scheme check, and on to the address check")
	require.False(t, r.sent)
}

// Whether an attempt may have reached the receiver is taken from whether
// its headers were written: a refused connection did not; a server that
// takes the request and never answers may have acted.
func TestAnAttemptIsSentOnceItsHeadersAreWritten(t *testing.T) {
	s := &Service{client: newClient(true), allowPrivate: true}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	r := s.post(context.Background(), url, uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.Error(t, r.err)
	require.False(t, r.sent, "nothing listened: proven unsent")

	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang)
	r = s.post(context.Background(), slow.URL, uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.Error(t, r.err)
	require.True(t, r.sent, "it arrived and timed out: possibly done")
}

// Only an explicit answer is final.
func TestOnlyAnExplicitAnswerIsFinal(t *testing.T) {
	for name, c := range map[string]struct {
		status  int
		body    string
		outcome string
	}{
		"done":                {200, `{"outcome":"done","result":{"id":"R-1"}}`, StatusDone},
		"done without result": {200, `{"outcome":"done"}`, StatusDone},
		"refused with 422":    {422, `{"outcome":"refused","refusal":{"reason":"no such order"}}`, StatusRefused},
		"refused with 200":    {200, `{"outcome":"refused","refusal":{"reason":"no"}}`, StatusRefused},
		"unknown":             {200, `{"outcome":"unknown","reason":"the bank timed out"}`, StatusUnknown},
		"a proxy's 404":       {404, `<html>not found</html>`, ""},
		"a bare 404 json":     {404, `{"error":"no route"}`, ""},
		"a 2xx without one":   {200, `{"id":"R-1"}`, ""},
		"done with a 500":     {500, `{"outcome":"done"}`, ""},
		"unknown with a 409":  {409, `{"outcome":"unknown"}`, ""},
		"a redirect":          {307, ``, ""},
		"something else":      {200, `{"outcome":"maybe"}`, ""},
	} {
		outcome, _, _ := explicitAnswer(c.status, []byte(c.body))
		require.Equal(t, c.outcome, outcome, name)
	}
	_, _, kept := explicitAnswer(200, []byte(`{"outcome":"done","result":{"id":"R-1"}}`))
	require.JSONEq(t, `{"id":"R-1"}`, string(kept))
	_, _, kept = explicitAnswer(422, []byte(`{"outcome":"refused","refusal":{"reason":"x"}}`))
	require.JSONEq(t, `{"reason":"x"}`, string(kept))
}

// The chain's resultSha256 is the digest of what the receiver answered,
// whether or not it could be stored, and says when it was not: it never
// digests the empty object an unknown answer is stored as instead.
func TestTheResultDigestIsOfTheReceiversAnswer(t *testing.T) {
	digestOf := func(raw string) string {
		digest, err := events.Digest(json.RawMessage(raw))
		require.NoError(t, err)
		return digest
	}
	huge := `1e2000`
	for name, c := range map[string]struct {
		status   int
		body     string
		answered string
		kept     bool
	}{
		"a done result kept":             {200, `{"outcome":"done","result":{"id":"R-1", "n": 2}}`, `{"id":"R-1","n":2}`, true},
		"a refusal kept":                 {422, `{"outcome":"refused","refusal":{"reason":"x"}}`, `{"reason":"x"}`, true},
		"an unknown answer kept":         {200, `{"outcome":"unknown","note":"timeout"}`, `{"outcome":"unknown","note":"timeout"}`, true},
		"a done result too big to keep":  {200, `{"outcome":"done","result":{"n":` + huge + `}}`, `{"n":` + huge + `}`, false},
		"a done result that is a string": {200, `{"outcome":"done","result":"R-1"}`, `"R-1"`, false},
		"an unknown answer too big":      {200, `{"outcome":"unknown","n":` + huge + `}`, `{"outcome":"unknown","n":` + huge + `}`, false},
	} {
		outcome, answered, kept := explicitAnswer(c.status, []byte(c.body))
		require.NotEmpty(t, outcome, name)
		fields := map[string]any{}
		addDigests(fields, attemptResult{outcome: outcome, answered: answered, kept: kept}, nil)
		require.Equal(t, digestOf(c.answered), fields["resultSha256"], name)
		require.NotEqual(t, digestOf(`{}`), fields["resultSha256"], name)
		if c.kept {
			require.NotContains(t, fields, "resultKept", name)
			require.Equal(t, digestOf(string(kept)), fields["resultSha256"], "%s: the same digest as what is stored", name)
		} else {
			require.Equal(t, false, fields["resultKept"], name)
		}
	}
	_, answered, _ := explicitAnswer(200, []byte(`{"outcome":"done"}`))
	fields := map[string]any{}
	addDigests(fields, attemptResult{answered: answered}, nil)
	require.NotContains(t, fields, "resultSha256", "no result, no digest")
}

// A retry backs off as River does, but never past the last moment an
// attempt may open, so a delivery gets every try its deadline allows.
func TestNextRetryStopsAtTheLastMomentAnAttemptMayOpen(t *testing.T) {
	w := &worker{}
	// River backs off by the number of recorded errors: five give 6^4
	// seconds, about 22 minutes, with up to 10% jitter either way.
	job := func(notAfter time.Time) *river.Job[DeliverArgs] {
		return &river.Job[DeliverArgs]{
			JobRow: &rivertype.JobRow{Attempt: 6, Errors: make([]rivertype.AttemptError, 5)},
			Args:   DeliverArgs{NotAfter: notAfter},
		}
	}
	backoffMin, backoffMax := 1166*time.Second, 1426*time.Second

	// A deadline before the backoff still gets one real try: strictly
	// before the last moment an attempt may open, with room for queue
	// latency, and still ahead.
	soon := time.Now().Add(time.Minute)
	before := time.Now()
	next := w.NextRetry(job(soon))
	sendAgainBefore := Delivery{NotAfter: soon}.SendAgainBefore()
	require.Equal(t, sendAgainBefore.Add(-RequestTimeout), next, "a deadline before the backoff leaves one real try")
	require.True(t, next.Before(sendAgainBefore), "the final try opens before attempts are refused")
	require.True(t, next.After(before), "the final try is still ahead")

	// Once that try is behind, the retry lands on the last moment, where
	// the delivery finishes, rather than looping at once.
	near := time.Now().Add(RequestTimeout + RequestTimeout/2)
	require.Equal(t, Delivery{NotAfter: near}.SendAgainBefore(), w.NextRetry(job(near)), "past the final try, the retry finishes the delivery")
	gone := time.Now().Add(-time.Minute)
	require.Equal(t, Delivery{NotAfter: gone}.SendAgainBefore(), w.NextRetry(job(gone)), "a passed deadline finishes the delivery")

	later := time.Now().Add(24 * time.Hour)
	before = time.Now()
	next = w.NextRetry(job(later))
	require.True(t, next.Before(later.Add(-RequestTimeout)), "a far deadline leaves the backoff alone")
	require.False(t, next.Before(before.Add(backoffMin)), "the default backoff, not sooner")
	require.False(t, next.After(time.Now().Add(backoffMax)), "the default backoff, not later")

	before = time.Now()
	next = w.NextRetry(job(time.Time{}))
	require.False(t, next.Before(before.Add(backoffMin)), "no deadline, no cap")
	require.False(t, next.After(time.Now().Add(backoffMax)), "no deadline, no cap")
}
