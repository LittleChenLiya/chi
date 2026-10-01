package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// Request signature headers. See request_signature.md for the full protocol
// description.
const (
	SignatureKeyIDHeader     = "X-Signature-Key-Id"
	SignatureTimestampHeader = "X-Signature-Timestamp"
	SignatureNonceHeader     = "X-Signature-Nonce"
	SignatureHeader          = "X-Signature"
)

// NonceStore tracks nonce values to prevent replay attacks. Implementations
// must be safe for concurrent use; Register in particular must be atomic.
type NonceStore interface {
	// Register atomically records the nonce for keyID if it is not already
	// registered, returning true. It returns false when the nonce is already
	// known, which indicates a replay. expiresAt tells the store when the
	// entry may be discarded.
	Register(keyID, nonce string, expiresAt time.Time) bool
	// Consume marks a previously registered nonce as consumed. It is only
	// called after the request signature has been fully verified.
	Consume(keyID, nonce string)
	// Release drops a previously registered nonce whose request failed
	// verification, so the client may retry with the same nonce.
	Release(keyID, nonce string)
}

// RequestSignatureConfig configures the RequestSignature middleware.
type RequestSignatureConfig struct {
	// KeyLookup returns the shared secret for a key id. Required.
	KeyLookup func(keyID string) (key []byte, ok bool)
	// NonceStore records nonces for replay protection. Required.
	NonceStore NonceStore
	// Clock supplies the current time for the timestamp window check.
	// Defaults to time.Now.
	Clock func() time.Time
	// TimeWindow is the maximum accepted clock skew between the request
	// timestamp and the local clock. Defaults to 5 minutes.
	TimeWindow time.Duration
	// MaxBodySize is the largest request body accepted for verification.
	// Larger bodies are rejected with 413. Defaults to 10 MiB.
	MaxBodySize int64
	// MemoryThreshold is the body size up to which bodies are buffered in
	// memory; larger bodies are spooled to a temporary file which is removed
	// once the request completes. Defaults to 1 MiB.
	MemoryThreshold int64
	// TempDir is the directory used for spooled bodies. Defaults to
	// os.TempDir().
	TempDir string
}

func (cfg *RequestSignatureConfig) withDefaults() {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.TimeWindow <= 0 {
		cfg.TimeWindow = 5 * time.Minute
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 10 << 20
	}
	if cfg.MemoryThreshold <= 0 {
		cfg.MemoryThreshold = 1 << 20
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
}

// RequestSignature returns a middleware that verifies HMAC-SHA256 request
// signatures. The signed string is:
//
//	method + "\n" + escaped path + "\n" + canonical query + "\n" +
//	timestamp + "\n" + nonce + "\n" + hex(sha256(body))
//
// The client sends the hex-encoded HMAC in the X-Signature header along with
// X-Signature-Key-Id, X-Signature-Timestamp (Unix seconds) and
// X-Signature-Nonce. The request is only passed to the next handler after
// the timestamp window, nonce and signature have all been verified; the body
// is restored so downstream handlers can read it in full. Error responses
// never reveal key material or computed signatures.
func RequestSignature(cfg RequestSignatureConfig) func(http.Handler) http.Handler {
	if cfg.KeyLookup == nil {
		panic("middleware: RequestSignature requires a KeyLookup")
	}
	if cfg.NonceStore == nil {
		panic("middleware: RequestSignature requires a NonceStore")
	}
	cfg.withDefaults()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keyID := r.Header.Get(SignatureKeyIDHeader)
			timestamp := r.Header.Get(SignatureTimestampHeader)
			nonce := r.Header.Get(SignatureNonceHeader)
			signature := r.Header.Get(SignatureHeader)
			if keyID == "" || timestamp == "" || nonce == "" || signature == "" {
				signatureUnauthorized(w)
				return
			}

			key, ok := cfg.KeyLookup(keyID)
			if !ok {
				signatureUnauthorized(w)
				return
			}

			tsUnix, err := strconv.ParseInt(timestamp, 10, 64)
			if err != nil {
				signatureUnauthorized(w)
				return
			}
			now := cfg.Clock()
			ts := time.Unix(tsUnix, 0)
			if ts.Before(now.Add(-cfg.TimeWindow)) || ts.After(now.Add(cfg.TimeWindow)) {
				signatureUnauthorized(w)
				return
			}

			gotSig, err := hex.DecodeString(signature)
			if err != nil || len(gotSig) != sha256.Size {
				signatureUnauthorized(w)
				return
			}

			bodyHash, restore, cleanup, tooLarge, err := spoolSignatureBody(r, cfg.MaxBodySize, cfg.MemoryThreshold, cfg.TempDir)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			if tooLarge {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			if cleanup != nil {
				defer cleanup()
			}

			if !cfg.NonceStore.Register(keyID, nonce, now.Add(cfg.TimeWindow)) {
				signatureUnauthorized(w)
				return
			}

			canonicalQuery, err := CanonicalQuery(r.URL.RawQuery)
			if err != nil {
				cfg.NonceStore.Release(keyID, nonce)
				http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
				return
			}

			escapedPath := r.URL.EscapedPath()
			if escapedPath == "" {
				escapedPath = "/"
			}
			stringToSign := StringToSign(r.Method, escapedPath, canonicalQuery, timestamp, nonce, bodyHash)
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(stringToSign))
			if !hmac.Equal(gotSig, mac.Sum(nil)) {
				cfg.NonceStore.Release(keyID, nonce)
				signatureUnauthorized(w)
				return
			}

			cfg.NonceStore.Consume(keyID, nonce)
			restore(r)
			next.ServeHTTP(w, r)
		})
	}
}

func signatureUnauthorized(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

// spoolSignatureBody reads r.Body up to maxBodySize bytes while hashing it.
// Bodies up to memoryThreshold are kept in memory; larger ones are spooled to
// a temporary file. It returns the body SHA-256, a restore func that resets
// r.Body so downstream handlers can read the full body, and a cleanup func
// (nil for in-memory bodies) that removes the temporary file.
func spoolSignatureBody(r *http.Request, maxBodySize, memoryThreshold int64, tempDir string) (hash [sha256.Size]byte, restore func(*http.Request), cleanup func(), tooLarge bool, err error) {
	hasher := sha256.New()
	if r.Body == nil {
		copy(hash[:], hasher.Sum(nil))
		return hash, func(req *http.Request) { req.Body = http.NoBody }, nil, false, nil
	}
	defer r.Body.Close()

	limited := io.LimitReader(r.Body, maxBodySize+1)
	var mem bytes.Buffer
	var tmp *os.File
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			total += int64(n)
			if total > maxBodySize {
				if tmp != nil {
					tmp.Close()
					os.Remove(tmp.Name())
				}
				return hash, nil, nil, true, nil
			}
			hasher.Write(chunk)
			if tmp != nil {
				if _, err = tmp.Write(chunk); err != nil {
					tmp.Close()
					os.Remove(tmp.Name())
					return hash, nil, nil, false, err
				}
			} else {
				mem.Write(chunk)
				if int64(mem.Len()) > memoryThreshold {
					tmp, err = os.CreateTemp(tempDir, "chi-request-signature-*")
					if err != nil {
						return hash, nil, nil, false, err
					}
					if _, err = tmp.Write(mem.Bytes()); err != nil {
						tmp.Close()
						os.Remove(tmp.Name())
						return hash, nil, nil, false, err
					}
					mem.Reset()
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if tmp != nil {
				tmp.Close()
				os.Remove(tmp.Name())
			}
			return hash, nil, nil, false, readErr
		}
	}
	copy(hash[:], hasher.Sum(nil))

	if tmp != nil {
		if _, err = tmp.Seek(0, io.SeekStart); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return hash, nil, nil, false, err
		}
		restore = func(req *http.Request) {
			req.Body = tmp
			req.ContentLength = total
		}
		cleanup = func() {
			tmp.Close()
			os.Remove(tmp.Name())
		}
		return hash, restore, cleanup, false, nil
	}

	body := mem.Bytes()
	restore = func(req *http.Request) {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = total
	}
	return hash, restore, nil, false, nil
}

// StringToSign builds the canonical string that is HMAC-SHA256 signed.
func StringToSign(method, escapedPath, canonicalQuery, timestamp, nonce string, bodyHash [sha256.Size]byte) string {
	return strings.Join([]string{
		method,
		escapedPath,
		canonicalQuery,
		timestamp,
		nonce,
		hex.EncodeToString(bodyHash[:]),
	}, "\n")
}

// SignString computes the hex-encoded HMAC-SHA256 signature of stringToSign.
func SignString(key []byte, stringToSign string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignRequest signs req for the given body and sets the signature headers.
// It is a client-side helper mirroring what the middleware verifies.
func SignRequest(key []byte, keyID, nonce string, timestamp time.Time, req *http.Request, body []byte) {
	bodyHash := sha256.Sum256(body)
	ts := strconv.FormatInt(timestamp.Unix(), 10)
	canonicalQuery, _ := CanonicalQuery(req.URL.RawQuery)
	escapedPath := req.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}
	signature := SignString(key, StringToSign(req.Method, escapedPath, canonicalQuery, ts, nonce, bodyHash))
	req.Header.Set(SignatureKeyIDHeader, keyID)
	req.Header.Set(SignatureTimestampHeader, ts)
	req.Header.Set(SignatureNonceHeader, nonce)
	req.Header.Set(SignatureHeader, signature)
}

// CanonicalQuery builds the canonical form of a raw query string: pairs are
// decoded, sorted by decoded key then decoded value, and re-encoded with a
// fixed percent-encoding (RFC 3986 unreserved characters are kept as-is,
// everything else becomes %XX with uppercase hex). Duplicate keys keep all
// their values and empty values are preserved.
func CanonicalQuery(rawQuery string) (string, error) {
	if rawQuery == "" {
		return "", nil
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", err
	}
	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(values))
	for key, vals := range values {
		for _, v := range vals {
			pairs = append(pairs, pair{key, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(percentEncode(p.key))
		sb.WriteByte('=')
		sb.WriteString(percentEncode(p.value))
	}
	return sb.String(), nil
}

// percentEncode encodes s keeping only RFC 3986 unreserved characters
// unescaped; all other bytes become %XX with uppercase hex digits.
func percentEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			sb.WriteByte(c)
		} else {
			const hexDigits = "0123456789ABCDEF"
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0x0f])
		}
	}
	return sb.String()
}

type nonceEntry struct {
	expiresAt time.Time
	consumed  bool
}

// memoryNonceStore is a NonceStore backed by an in-memory map.
type memoryNonceStore struct {
	mu      sync.Mutex
	entries map[string]nonceEntry
	clock   func() time.Time
}

// NewMemoryNonceStore returns a simple in-memory NonceStore suitable for a
// single-process server. An optional clock may be supplied to control expiry;
// it defaults to time.Now.
func NewMemoryNonceStore(clock ...func() time.Time) NonceStore {
	s := &memoryNonceStore{entries: make(map[string]nonceEntry), clock: time.Now}
	if len(clock) > 0 && clock[0] != nil {
		s.clock = clock[0]
	}
	return s
}

func nonceKey(keyID, nonce string) string {
	return keyID + "\x00" + nonce
}

func (s *memoryNonceStore) Register(keyID, nonce string, expiresAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
	key := nonceKey(keyID, nonce)
	if _, exists := s.entries[key]; exists {
		return false
	}
	s.entries[key] = nonceEntry{expiresAt: expiresAt}
	return true
}

func (s *memoryNonceStore) Consume(keyID, nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := nonceKey(keyID, nonce)
	if e, exists := s.entries[key]; exists {
		e.consumed = true
		s.entries[key] = e
	}
}

func (s *memoryNonceStore) Release(keyID, nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, nonceKey(keyID, nonce))
}
