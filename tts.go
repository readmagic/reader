package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// discard drains and closes r so the connection can be reused.
func discard(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, r)
	_ = r.Close()
}

// Voice presets carried over from the previous text-to-speech.cn setup.
// Edge TTS uses the same voice id namespace, so the default stays
// zh-CN-XiaoxiaoNeural.
const (
	defaultVoice    = "zh-CN-XiaoxiaoNeural"
	defaultFormat   = "audio-24khz-48kbitrate-mono-mp3"
	endpointURL     = "https://dev.microsofttranslator.com/apps/endpoint?api-version=1.0"
	tokenRefreshSlack = 180 // seconds before expiry to refresh
)

// signKey is the HMAC key embedded in the MSTranslator Android client,
// recovered by the wangwangit/tts project. It is constant — every caller
// of the endpoint API uses the same key.
var signKey, _ = base64.StdEncoding.DecodeString(
	"oik6PdDdMnOXemTbwvMn9de/h9lFnfBaCWbGMMZqqoSaQaqUOqjVGm5NqsmjcBI1x+sS9ugjB55HEJWRiFXYFw==",
)

// endpoint is the JSON payload returned by the token API: t is the JWT
// bearer token, r is the azure region hostname prefix (e.g. "eastus").
type endpoint struct {
	T string `json:"t"`
	R string `json:"r"`
}

// ttsClient synthesizes speech via the Microsoft Edge TTS cognitive
// services endpoint, caching mp3 files on disk keyed by sha256(text).
type ttsClient struct {
	http  *http.Client
	cache string
	retry int

	mu       sync.Mutex
	ep       *endpoint
	epExpire int64
}

func newTTSClient(cacheDir string) *ttsClient {
	return &ttsClient{
		http: &http.Client{
			Timeout: 60 * time.Second,
			// Allow several concurrent synthesize calls to share idle
			// connections — the prefetcher fans out 2-3 at a time.
			Transport: &http.Transport{MaxIdleConnsPerHost: 4},
		},
		cache: cacheDir,
		retry: 3,
	}
}

// authErr marks permanent rejections (bad signature, revoked key).
// Retrying immediately cannot fix these.
type authErr struct{ msg string }

func (e authErr) Error() string { return e.msg }

// retryableErr wraps transient failures (429, 5xx, network) so the
// synthesis loop knows to back off and try again.
type retryableErr struct{ msg string }

func (e retryableErr) Error() string { return e.msg }

// synthesize returns a local mp3 path for text, fetching and caching
// via Edge TTS when not already on disk. Safe for concurrent callers —
// the endpoint token is shared under a mutex.
func (c *ttsClient) synthesize(text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	path := filepath.Join(c.cache, hex.EncodeToString(sum[:16])+".mp3")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	var lastErr error
	for attempt := 0; attempt <= c.retry; attempt++ {
		err := c.fetchOnce(text, path)
		if err == nil {
			return path, nil
		}
		lastErr = err
		if _, ok := err.(authErr); ok {
			break
		}
		if _, ok := err.(retryableErr); !ok {
			break
		}
		// Linear backoff: 0.5s, 1s, 1.5s ...
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return "", lastErr
}

func (c *ttsClient) fetchOnce(text, dest string) error {
	ep, err := c.getEndpoint()
	if err != nil {
		return err
	}

	body := buildSSML(text, defaultVoice, "+0%", "+0Hz", "+0%", "general", 0)
	url := fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", ep.R)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", ep.T)
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", defaultFormat)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36 Edg/127.0.0.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return retryableErr{msg: "edge tts network: " + err.Error()}
	}
	defer discard(resp.Body)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return retryableErr{msg: fmt.Sprintf("edge tts: status %d %s", resp.StatusCode, snippet)}
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Force a token refresh on next attempt.
			c.mu.Lock()
			c.ep = nil
			c.mu.Unlock()
			return retryableErr{msg: fmt.Sprintf("edge tts auth: status %d %s", resp.StatusCode, snippet)}
		default:
			return authErr{msg: fmt.Sprintf("edge tts: status %d %s", resp.StatusCode, snippet)}
		}
	}

	tmp, err := os.CreateTemp(c.cache, "tmp-*.mp3")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return retryableErr{msg: "download mp3: " + err.Error()}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// getEndpoint returns a cached token if still valid, otherwise refreshes
// under a mutex so concurrent synthesize calls share a single token.
func (c *ttsClient) getEndpoint() (*endpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().Unix()
	if c.ep != nil && now < c.epExpire-tokenRefreshSlack {
		return c.ep, nil
	}

	sig, err := sign(endpointURL)
	if err != nil {
		return nil, err
	}
	traceID, err := newUUID()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpointURL, strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "zh-Hans")
	req.Header.Set("X-ClientVersion", "4.0.530a 5fe1dc6c")
	req.Header.Set("X-UserId", "0f04d16a175c411e")
	req.Header.Set("X-HomeGeographicRegion", "zh-Hans-CN")
	req.Header.Set("X-ClientTraceId", traceID)
	req.Header.Set("X-MT-Signature", sig)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36 Edg/127.0.0.0")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, retryableErr{msg: "endpoint api: " + err.Error()}
	}
	defer discard(resp.Body)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("endpoint api: status %d %s", resp.StatusCode, snippet)
	}

	var ep endpoint
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return nil, fmt.Errorf("endpoint decode: %w", err)
	}
	if ep.T == "" || ep.R == "" {
		return nil, fmt.Errorf("endpoint api: empty token or region")
	}
	c.ep = &ep
	c.epExpire = jwtExpiry(ep.T)
	return &ep, nil
}

// jwtExpiry extracts the `exp` claim from a JWT without verifying it.
// Edge tokens are short-lived; we just need the timestamp.
func jwtExpiry(jwt string) int64 {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		// Fall back to "now + 5 min" so we still try to use the token.
		return time.Now().Unix() + 300
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Now().Unix() + 300
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Now().Unix() + 300
	}
	return claims.Exp
}

// newUUID returns a v4 UUID in the canonical 8-4-4-4-12 form.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// sign produces the X-MT-Signature header value for the given URL.
// The algorithm mirrors the MSTranslatorAndroidApp client recovered in
// wangwangit/tts/index.js.
func sign(rawURL string) (string, error) {
	parts := strings.SplitN(rawURL, "://", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("sign: invalid url %q", rawURL)
	}
	encodedURL := encodeURIComponent(parts[1])
	traceUUID, err := newUUID()
	if err != nil {
		return "", err
	}
	traceUUID = strings.ReplaceAll(traceUUID, "-", "")
	formattedDate := utcHeader()
	bytesToSign := strings.ToLower("MSTranslatorAndroidApp" + encodedURL + formattedDate + traceUUID)

	mac := hmac.New(sha256.New, signKey)
	mac.Write([]byte(bytesToSign))
	sigB64 := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return "MSTranslatorAndroidApp::" + sigB64 + "::" + formattedDate + "::" + traceUUID, nil
}

// utcHeader formats now as the lowercase UTC string Edge expects,
// matching JS `(new Date()).toUTCString().replace(/GMT/, "").trim() + " GMT"`,
// then `.toLowerCase()`.
func utcHeader() string {
	return strings.ToLower(time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT")
}

// encodeURIComponent mirrors JS encodeURIComponent: ASCII unreserved
// characters pass through, everything else is %XX UTF-8 encoded.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			switch {
			case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', '0' <= r && r <= '9',
				r == '-', r == '_', r == '.', r == '!', r == '~', r == '*', r == '\'', r == '(', r == ')':
				b.WriteRune(r)
			default:
				bs := byte(r)
				b.WriteByte('%')
				b.WriteByte(hex[bs>>4])
				b.WriteByte(hex[bs&0x0f])
			}
			continue
		}
		for _, bs := range []byte(string(r)) {
			b.WriteByte('%')
			b.WriteByte(hex[bs>>4])
			b.WriteByte(hex[bs&0x0f])
		}
	}
	return b.String()
}

// buildSSML constructs the SSML body Edge expects. The structure mirrors
// the wangwangit/tts getSsml output verbatim so the service treats us
// exactly like the reference Android client.
func buildSSML(text, voice, rate, pitch, volume, style string, slienMS int) string {
	escaped := escapeXML(text)
	silence := ""
	if slienMS > 0 {
		silence = fmt.Sprintf(`<break time="%dms" />`, slienMS)
	}
	return fmt.Sprintf(
		`<speak xmlns="http://www.w3.org/2001/10/synthesis" xmlns:mstts="http://www.w3.org/2001/mstts" version="1.0" xml:lang="zh-CN"><voice name="%s"><mstts:express-as style="%s" styledegree="2.0" role="default"><prosody rate="%s" pitch="%s" volume="%s">%s</prosody></mstts:express-as>%s</voice></speak>`,
		voice, style, rate, pitch, volume, escaped, silence,
	)
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
