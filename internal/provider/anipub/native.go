package anipub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	embedRE  = regexp.MustCompile(`(?i)<iframe\b[^>]*\bsrc\s*=\s*["']?([^\s"'<>]+)`)
	playerRE = regexp.MustCompile(`(?i)<div\b[^>]*\bid=["']megaplay-player["'][^>]*>`)
	fileIDRE = regexp.MustCompile(`(?i)\bdata-id=["'](\d+)["']`)
	clientRE = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc=["']([^"']*/newclient\.min\.js[^"']*)["']`)
	encodeRE = regexp.MustCompile(`\.encode\(("(?:\\.|[^"\\])*")\)`)
)

type sourceCipher struct {
	key [32]byte
	iv  [aes.BlockSize]byte
}

// MegaPlay publishes its source/segment URL cipher in newclient.min.js. Read
// those parameters on each resolution so rotating them doesn't require a new
// plugin build. This parser deliberately refuses an unfamiliar script shape;
// the optional browser fallback can still resolve the site's own player.
func parseSourceCipher(script string) (*sourceCipher, error) {
	values := encodeRE.FindAllStringSubmatch(script, -1)
	if len(values) != 2 || !strings.Contains(script, `"AES-CBC"`) {
		return nil, errors.New("unsupported MegaPlay cipher script")
	}
	var key, iv string
	if json.Unmarshal([]byte(values[0][1]), &key) != nil || json.Unmarshal([]byte(values[1][1]), &iv) != nil ||
		len(key) == 0 || len(key) > 32 || len(iv) != aes.BlockSize {
		return nil, errors.New("invalid MegaPlay cipher parameters")
	}
	c := &sourceCipher{}
	copy(c.key[:], key)
	copy(c.iv[:], iv)
	return c, nil
}

func (c *sourceCipher) decrypt(token string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("MegaPlay segment needs URL decryption")
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil || len(body) == 0 || len(body)%aes.BlockSize != 0 {
		return nil, errors.New("invalid MegaPlay encrypted URL")
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, err
	}
	cipher.NewCBCDecrypter(block, c.iv[:]).CryptBlocks(body, body)
	padding := int(body[len(body)-1])
	if padding < 1 || padding > aes.BlockSize {
		return nil, errors.New("invalid MegaPlay URL padding")
	}
	for _, value := range body[len(body)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid MegaPlay URL padding")
		}
	}
	return body[:len(body)-padding], nil
}

func (c *Client) nativeStreams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	watch := watchURL(c.base, episode.ShowID, episode.Value, translation)
	page, pageURL, err := c.resource(ctx, watch, map[string]string{"Referer": c.base + "/"})
	if err != nil {
		return nil, fmt.Errorf("load AniPub player: %w", err)
	}
	match := embedRE.FindStringSubmatch(string(page))
	if match == nil {
		return nil, errors.New("AniPub player iframe not found")
	}
	embed, err := resourceURL(pageURL, html.UnescapeString(match[1]))
	if err != nil {
		return nil, err
	}
	page, embed, err = c.resource(ctx, embed, map[string]string{"Referer": pageURL})
	if err != nil {
		return nil, fmt.Errorf("load MegaPlay embed: %w", err)
	}
	id := fileIDRE.FindStringSubmatch(playerRE.FindString(string(page)))
	script := clientRE.FindStringSubmatch(string(page))
	if id == nil || script == nil {
		return nil, errors.New("unsupported MegaPlay player page")
	}
	scriptURL, err := resourceURL(embed, html.UnescapeString(script[1]))
	if err != nil {
		return nil, err
	}
	params, _, err := c.resource(ctx, scriptURL, map[string]string{"Referer": embed})
	if err != nil {
		return nil, err
	}
	codec, err := parseSourceCipher(string(params))
	if err != nil {
		return nil, err
	}
	endpoint, err := resourceURL(embed, "/stream/getSources?id="+id[1])
	if err != nil {
		return nil, err
	}
	body, _, err := c.resource(ctx, endpoint, map[string]string{"Referer": embed, "X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return nil, err
	}
	var response struct {
		Encrypted string `json:"enc"`
		Tracks    []struct {
			File  string `json:"file"`
			Label string `json:"label"`
			Kind  string `json:"kind"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode MegaPlay source: %w", err)
	}
	plain, err := codec.decrypt(response.Encrypted)
	if err != nil {
		return nil, err
	}
	var source struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(plain, &source); err != nil {
		return nil, errors.New("invalid decrypted MegaPlay source")
	}
	media, err := resourceURL(embed, source.File)
	if err != nil {
		return nil, err
	}
	origin, _ := url.Parse(embed)
	stream := Stream{
		URL: media, Source: "native",
		Headers: map[string]string{"Referer": origin.Scheme + "://" + origin.Host + "/", "User-Agent": userAgent},
	}
	if translation == "sub" {
		for _, track := range response.Tracks {
			if strings.EqualFold(track.Label, "English") && track.Kind == "captions" {
				stream.Subtitle, err = resourceURL(embed, track.File)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		if stream.Subtitle == "" {
			return nil, errors.New("MegaPlay source has no full English subtitle track")
		}
	}
	stream, err = c.prepareStream(ctx, stream, quality, codec)
	if err != nil {
		return nil, err
	}
	return []Stream{stream}, nil
}

func resourceURL(base, reference string) (string, error) {
	if strings.TrimSpace(reference) == "" || strings.ContainsAny(reference, "\r\n\t") {
		return "", errors.New("empty or invalid AniPub resource URL")
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", err
	}
	u = b.ResolveReference(u)
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("unsupported AniPub resource URL")
	}
	return u.String(), nil
}

func (c *Client) resource(ctx context.Context, endpoint string, headers map[string]string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("AniPub resource HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxBody {
		return nil, "", errors.New("AniPub resource exceeds size limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return body, resp.Request.URL.String(), nil
}
