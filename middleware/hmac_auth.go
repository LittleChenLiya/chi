package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HMAC-SHA256 request signing protocol
//
// The HMACAuth middleware verifies that a request was signed by a client
// holding the secret key associated with a key-id. The client computes
//
//	signature = hex(HMAC-SHA256(secret, signingString))
//
// where signingString is the concatenation of the following lines, each
// terminated by a single "\n" (including the last one):
//
//	<HTTP method, upper case>
//	<raw escaped path, e.g. /files/a%2Fb — never the decoded path>
//	<canonical query string, see CanonicalHMACQuery>
//	<unix timestamp in seconds, decimal>
//	<nonce, unique per key-id within the time window>
//	<lowercase hex SHA-256 of the exact request body>
//
// The client sends the key-id, timestamp, nonce and signature in headers:
//
//	X-Key-Id:     opaque identifier resolved to a secret by the HMACKeyStore
//	X-Timestamp:  unix seconds; rejected outside +/- HMACAuthConfig.TimeWindow
//	X-Nonce:      single-use token, atomically registered via the NonceStore
//	X-Signature:  lowercase hex of the HMAC-SHA256 described above
//
// Canonical query: the raw query is split on "&", every key and value is
// percent-decoded, pairs are sorted by decoded key then decoded value
// (byte-wise), and re-encoded with a fixed percent-encoding (unreserved
// characters [A-Za-z0-9-._~] pass through, everything else is emitted as
// uppercase %XX, space as %20). Repeated parameters keep every occurrence
// and empty values participate as "key=".
//
// The middleware buffers the request body before any handler runs: bodies
// up to MemoryBodyBytes stay in memory, larger bodies are spooled to a
// temporary file which is removed once the request completes. Bodies
// larger than MaxBodyBytes are rejected with 413. The nonce is consumed
// (registered in the NonceStore) only after the signature has been fully
// verified, so failed attempts never burn a nonce. Error responses never
// echo the key, the expected signature or any derived material. On success
// the downstream handler can still read the complete request body.

// HMAC authentication header names.
const (
	HMACKeyIDHeader     = "X-Key-Id"
	HMACTimestampHeader = "X-Timestamp"
	HMACNonceHeader     = "X-Nonce"
	HMACSignatureHeader = "X-Signature"
)

// HMACKeyStore resolves a key-id to its secret key. ok is false when the
// key-id is unknown.
type HMACKeyStore interface {
	SecretKey(keyID string) (secret []byte, ok bool)
}

// HMACKeyFunc adapts a plain function to HMACKeyStore.
type HMACKeyFunc func(keyID string) (secret []byte, ok bool)

// SecretKey implements HMACKeyStore.
func (f HMACKeyFunc) SecretKey(keyID string) ([]byte, bool) { return f(keyID) }

// NonceStore atomically registers single-use nonces.
type NonceStore interface {
	// RegisterIfAbsent records nonce for keyID, valid until expiresAt, and
	// returns true. If the nonce was already registered (and not yet
	// expired) it returns false and changes nothing. Implementations must
	// be safe for concurrent use and atomic: exactly one of any number of
	// concurrent calls with the same key-id/nonce pair may return true.
	RegisterIfAbsent(keyID, nonce string, expiresAt time.Time) bool
}

// MemoryNonceStore is an in-process NonceStore backed by a map. It is
// suitable for single-process deployments and tests.
type MemoryNonceStore struct {
	mu     sync.Mutex
	nonces map[string]time.Time
}

// NewMemoryNonceStore returns an empty in-memory nonce store.
func NewMemoryNonceStore() *MemoryNonceStore {
	return &MemoryNonceStore{nonces: make(map[string]time.Time)}
}

// RegisterIfAbsent implements NonceStore.
func (s *MemoryNonceStore) RegisterIfAbsent(keyID, nonce string, expiresAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.nonces {
		if now.After(exp) {
			delete(s.nonces, k)
		}
	}
	key := keyID + "\x00" + nonce
	if _, exists := s.nonces[key]; exists {
		return false
	}
	s.nonces[key] = expiresAt
	return true
}

// HMACAuthConfig configures the HMACAuth middleware.
type HMACAuthConfig struct {
	// KeyStore resolves key-ids to secret keys. Required.
	KeyStore HMACKeyStore
	// NonceStore registers single-use nonces. Defaults to a new
	// MemoryNonceStore.
	NonceStore NonceStore
	// Clock supplies the current time; injectable for tests.
	// Defaults to time.Now.
	Clock func() time.Time
	// TimeWindow is the maximum accepted clock skew between the
	// X-Timestamp header and the server clock. Defaults to 5 minutes.
	TimeWindow time.Duration
	// MaxBodyBytes is the largest accepted request body; larger requests
	// are rejected with 413 before any signature work. Defaults to 1 MiB.
	MaxBodyBytes int64
	// MemoryBodyBytes is the threshold above which the buffered body is
	// spooled to a temporary file instead of memory. Defaults to 64 KiB.
	MemoryBodyBytes int64
	// TempDir receives the spool files for large bodies.
	// Defaults to os.TempDir().
	TempDir string
}

func (c *HMACAuthConfig) withDefaults() HMACAuthConfig {
	out := *c
	if out.NonceStore == nil {
		out.NonceStore = NewMemoryNonceStore()
	}
	if out.Clock == nil {
		out.Clock = time.Now
	}
	if out.TimeWindow <= 0 {
		out.TimeWindow = 5 * time.Minute
	}
	if out.MaxBodyBytes <= 0 {
		out.MaxBodyBytes = 1 << 20
	}
	if out.MemoryBodyBytes <= 0 {
		out.MemoryBodyBytes = 64 << 10
	}
	if out.MemoryBodyBytes > out.MaxBodyBytes {
		out.MemoryBodyBytes = out.MaxBodyBytes
	}
	if out.TempDir == "" {
		out.TempDir = os.TempDir()
	}
	return out
}

// HMACAuth returns a middleware that verifies HMAC-SHA256 signed requests
// as described in the package-level protocol documentation above. Only
// fully verified requests reach the next handler.
func HMACAuth(cfg HMACAuthConfig) func(next http.Handler) http.Handler {
	conf := cfg.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if conf.KeyStore == nil {
				// Misconfiguration is a server error, not an auth failure.
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}

			keyID := r.Header.Get(HMACKeyIDHeader)
			timestampHeader := r.Header.Get(HMACTimestampHeader)
			nonce := r.Header.Get(HMACNonceHeader)
			signatureHeader := r.Header.Get(HMACSignatureHeader)
			if keyID == "" || timestampHeader == "" || nonce == "" || signatureHeader == "" {
				hmacUnauthorized(w)
				return
			}

			timestamp, err := strconv.ParseInt(timestampHeader, 10, 64)
			if err != nil {
				hmacUnauthorized(w)
				return
			}
			now := conf.Clock()
			skew := now.Sub(time.Unix(timestamp, 0))
			if skew < 0 {
				skew = -skew
			}
			if skew > conf.TimeWindow {
				hmacUnauthorized(w)
				return
			}

			secret, ok := conf.KeyStore.SecretKey(keyID)
			if !ok {
				hmacUnauthorized(w)
				return
			}

			signature, err := hex.DecodeString(signatureHeader)
			if err != nil {
				hmacUnauthorized(w)
				return
			}

			canonicalQuery, err := CanonicalHMACQuery(r.URL.RawQuery)
			if err != nil {
				hmacUnauthorized(w)
				return
			}

			bodyHash, replay, cleanup, err := conf.bufferBody(r)
			if err != nil {
				if errors.Is(err, errHMACBodyTooLarge) {
					http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				} else {
					hmacUnauthorized(w)
				}
				return
			}
			defer cleanup()

			expected := HMACHMAC(secret, HMACSigningString(r.Method, hmacEscapedPath(r), canonicalQuery, timestampHeader, nonce, bodyHash))
			if !hmac.Equal(signature, expected) {
				hmacUnauthorized(w)
				return
			}

			// The signature is valid; only now consume the nonce so that
			// failed attempts never burn one.
			if !conf.NonceStore.RegisterIfAbsent(keyID, nonce, now.Add(conf.TimeWindow)) {
				hmacUnauthorized(w)
				return
			}

			r.Body = replay
			next.ServeHTTP(w, r)
		})
	}
}

var errHMACBodyTooLarge = errors.New("hmacauth: request body too large")

func hmacUnauthorized(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func hmacEscapedPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// bufferBody reads the whole request body, hashing it on the fly. Bodies
// up to MemoryBodyBytes are kept in memory; larger ones are spooled to a
// temporary file. It returns the hex SHA-256 of the body, a fresh
// ReadCloser that replays the full body to the downstream handler, and a
// cleanup function that releases any temporary resources.
func (c HMACAuthConfig) bufferBody(r *http.Request) (hashHex string, replay io.ReadCloser, cleanup func(), err error) {
	cleanup = func() {}
	if r.Body == nil {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), http.NoBody, cleanup, nil
	}
	defer r.Body.Close()

	hasher := sha256.New()
	buf := make([]byte, 0, min(c.MemoryBodyBytes, 32<<10))
	var tmp *os.File
	chunk := make([]byte, 32<<10)
	var total int64

	fail := func(err error) (string, io.ReadCloser, func(), error) {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
		return "", nil, cleanup, err
	}

	for {
		n, readErr := r.Body.Read(chunk)
		if n > 0 {
			total += int64(n)
			if total > c.MaxBodyBytes {
				return fail(errHMACBodyTooLarge)
			}
			hasher.Write(chunk[:n])
			if tmp != nil {
				if _, werr := tmp.Write(chunk[:n]); werr != nil {
					return fail(werr)
				}
			} else if int64(len(buf))+int64(n) > c.MemoryBodyBytes {
				tmp, err = os.CreateTemp(c.TempDir, "hmacauth-body-*")
				if err != nil {
					return fail(err)
				}
				if _, werr := tmp.Write(buf); werr != nil {
					return fail(werr)
				}
				buf = nil
				if _, werr := tmp.Write(chunk[:n]); werr != nil {
					return fail(werr)
				}
			} else {
				buf = append(buf, chunk[:n]...)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fail(readErr)
		}
	}

	hashHex = hex.EncodeToString(hasher.Sum(nil))
	if tmp == nil {
		return hashHex, io.NopCloser(bytes.NewReader(buf)), cleanup, nil
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	name := tmp.Name()
	cleanup = func() {
		tmp.Close()
		os.Remove(name)
	}
	return hashHex, &hmacFileReplay{File: tmp}, cleanup, nil
}

// hmacFileReplay reads a spooled body from its temporary file; closing is
// a no-op because the middleware owns the file lifecycle.
type hmacFileReplay struct {
	*os.File
}

func (f *hmacFileReplay) Close() error { return nil }

// HMACSigningString builds the canonical string that is HMAC-signed, as
// documented at the top of this file. bodyHashHex is the lowercase hex
// SHA-256 of the exact request body.
func HMACSigningString(method, escapedPath, canonicalQuery, timestamp, nonce, bodyHashHex string) string {
	return strings.Join([]string{method, escapedPath, canonicalQuery, timestamp, nonce, bodyHashHex}, "\n") + "\n"
}

// HMACHMAC computes the HMAC-SHA256 of the signing string with secret.
func HMACHMAC(secret []byte, signingString string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingString))
	return mac.Sum(nil)
}

// CanonicalHMACQuery canonicalizes a raw query string for signing: pairs
// are percent-decoded, sorted by decoded key then decoded value, and
// re-encoded with a fixed percent-encoding. Repeated parameters keep every
// occurrence; empty values participate as "key=".
func CanonicalHMACQuery(rawQuery string) (string, error) {
	if rawQuery == "" {
		return "", nil
	}
	type pair struct{ key, value string }
	var pairs []pair
	for _, segment := range strings.Split(rawQuery, "&") {
		if segment == "" {
			continue
		}
		key, value, _ := strings.Cut(segment, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return "", fmt.Errorf("hmacauth: invalid query key: %w", err)
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return "", fmt.Errorf("hmacauth: invalid query value: %w", err)
		}
		pairs = append(pairs, pair{decodedKey, decodedValue})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(hmacPercentEncode(p.key))
		b.WriteByte('=')
		b.WriteString(hmacPercentEncode(p.value))
	}
	return b.String(), nil
}

// hmacPercentEncode encodes using the fixed percent-encoding of the
// protocol: RFC 3986 unreserved characters pass through, every other byte
// is emitted as uppercase %XX (space becomes %20, never "+").
func hmacPercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// SignHMACRequest is a client-side helper that signs a request according
// to the protocol, setting the X-Key-Id, X-Timestamp, X-Nonce and
// X-Signature headers. body must be the exact bytes sent as the request
// body. It is used by the tests and the demo, and may be used by clients.
func SignHMACRequest(req *http.Request, secret []byte, keyID, nonce string, timestamp time.Time, body []byte) error {
	canonicalQuery, err := CanonicalHMACQuery(req.URL.RawQuery)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	timestampHeader := strconv.FormatInt(timestamp.Unix(), 10)
	escapedPath := req.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}
	signature := HMACHMAC(secret, HMACSigningString(req.Method, escapedPath, canonicalQuery, timestampHeader, nonce, hex.EncodeToString(sum[:])))
	req.Header.Set(HMACKeyIDHeader, keyID)
	req.Header.Set(HMACTimestampHeader, timestampHeader)
	req.Header.Set(HMACNonceHeader, nonce)
	req.Header.Set(HMACSignatureHeader, hex.EncodeToString(signature))
	return nil
}
