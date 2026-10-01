# Request Signature Middleware

`RequestSignature` is a reusable chi middleware that authenticates
requests with HMAC-SHA256 signatures. The caller supplies keys by key id; the
middleware verifies the timestamp window, registers the nonce for replay
protection, hashes the body, and only hands the request to the next handler
after every check has passed.

## Protocol

The client computes a signature over a canonical string built from six
newline-separated fields:

    stringToSign = method + "\n" +
                   escapedPath + "\n" +
                   canonicalQuery + "\n" +
                   timestamp + "\n" +
                   nonce + "\n" +
                   hex(sha256(body))

    signature = hex(HMAC-SHA256(key, stringToSign))

- **method**: the HTTP method, exactly as sent (e.g. `POST`).
- **escapedPath**: the raw escaped request path (`url.URL.EscapedPath()`),
  e.g. `/items/a%2Fb`. The decoded form is never signed.
- **canonicalQuery**: the canonicalized query string (see below).
- **timestamp**: Unix seconds, as a decimal string.
- **nonce**: a unique-per-request opaque string chosen by the client.
- **body hash**: lowercase hex of the SHA-256 of the raw request body
  (the empty body hashes to the SHA-256 of zero bytes).

### Canonical query

1. Parse the raw query into key/value pairs, percent-decoding both (`+`
   decodes to a space).
2. Sort pairs by decoded key, then by decoded value (byte-wise).
3. Re-encode every key and value with a fixed percent-encoding: RFC 3986
   unreserved characters (`A-Z a-z 0-9 - . _ ~`) are kept as-is, every
   other byte becomes `%XX` with uppercase hex digits.
4. Join `key=value` pairs with `&`.

Duplicate keys keep all of their values as separate pairs, and empty values
participate in the signature (`a=`). For example
`b=2&a=1&a=` canonicalizes to `a=&a=1&b=2`.

### Request headers

| Header                  | Value                                  |
|-------------------------|----------------------------------------|
| `X-Signature-Key-Id`     | key id, resolved to a secret by the server |
| `X-Signature-Timestamp`  | Unix seconds                           |
| `X-Signature-Nonce`      | unique nonce                           |
| `X-Signature`            | hex-encoded HMAC-SHA256                |

## Server behavior

- **Keys** are resolved per request through `RequestSignatureConfig.KeyLookup`,
  so callers control storage and rotation.
- **Time window**: the timestamp must be within `TimeWindow` of the
  injected `Clock` (default 5 minutes), otherwise the request is
  rejected with 401.
- **Replay protection**: the nonce is registered atomically through the
  `NonceStore` interface. A nonce that is already registered is
  rejected with 401. The nonce is *consumed* only after the signature has
  been fully verified; failed verifications release the nonce so clients may
  retry.
- **Body handling**: bodies up to `MemoryThreshold` are buffered in
  memory; larger bodies are spooled to a temporary file in `TempDir`
  which is removed once the request completes. Bodies larger than
  `MaxBodySize` are rejected outright with 413.
- **Downstream reads**: after successful verification the body is restored,
  so handlers can read it in full.
- **Error responses** are generic (400/401/413) and never reveal key
  material or computed signatures.

## Usage

```go
r := chi.NewRouter()
r.Use(middleware.RequestSignature(middleware.RequestSignatureConfig{
    KeyLookup: func(keyID string) ([]byte, bool) {
        key, ok := keystore[keyID]
        return key, ok
    },
    NonceStore: middleware.NewMemoryNonceStore(),
}))
```

Clients can sign requests with the exported helpers `SignRequest`,
`StringToSign`, `SignString` and `CanonicalQuery`. See
`_examples/request-signature` for a runnable loopback demo.

