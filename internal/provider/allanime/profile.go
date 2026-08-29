package allanime

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type bundleDecoder struct {
	table  string
	offset int
}

type bundleAlias struct {
	base  string
	arg   int
	delta int
}

type bundleData struct {
	buildIDs []string
	seeds    []string
	rot      int
	salt     [4]int
	boot     string
	join     string
	parts    []string
}

var (
	bundleTableRE  = regexp.MustCompile(`(?s)function ([A-Za-z$_][A-Za-z0-9$_]*)\(\)\s*\{\s*(?:const|let|var)\s+[A-Za-z$_][A-Za-z0-9$_]*\s*=\s*(\[[^\]]*\]);`)
	bundleBaseRE   = regexp.MustCompile(`function ([A-Za-z$_][A-Za-z0-9$_]*)\(([A-Za-z$_][A-Za-z0-9$_]*)(?:,[A-Za-z$_][A-Za-z0-9$_]*)*\)\{return [A-Za-z$_][A-Za-z0-9$_]*=[A-Za-z$_][A-Za-z0-9$_]*-\(([-+*0-9 ]+)\),([A-Za-z$_][A-Za-z0-9$_]*)\(\)\[[A-Za-z$_][A-Za-z0-9$_]*\]\}`)
	bundleAliasRE  = regexp.MustCompile(`function ([A-Za-z$_][A-Za-z0-9$_]*)\(([A-Za-z$_][A-Za-z0-9$_]*),([A-Za-z$_][A-Za-z0-9$_]*)\)\{return ([A-Za-z$_][A-Za-z0-9$_]*)\(([A-Za-z$_][A-Za-z0-9$_]*)((?:[-+][+\-0-9* ]+)?)\)\}`)
	bundleCallRE   = regexp.MustCompile(`([A-Za-z$_][A-Za-z0-9$_]*)\(\s*(-?[0-9]+)\s*(?:,\s*(-?[0-9]+)\s*)?\)`)
	bundleNumberRE = regexp.MustCompile(`^[0-9]{2,8}$`)
	bundleSeedRE   = regexp.MustCompile(`^[A-Za-z0-9+/]{11}=$`)
	bundleEntryRE  = regexp.MustCompile(`import\("([^"]*/entry/app\.[^"]+\.js)"\)`)
	bundleChunkRE  = regexp.MustCompile(`['"](\.\.?/chunks/[A-Za-z0-9_$./-]+\.js)['"]`)
	bundleConfigRE = regexp.MustCompile(`(?s)[A-Za-z$_][A-Za-z0-9$_]*=\{([^}]*saltMul[^}]*bootPrefix[^}]*)\}`)
)

func (c *Client) refreshProfiles(ctx context.Context) ([]cryptoProfile, error) {
	html, err := c.fetchBundleText(ctx, strings.TrimSuffix(c.origin, "/")+"/")
	if err != nil {
		return nil, fmt.Errorf("fetch AllAnime site bundle: %w", err)
	}
	entry := bundleEntryRE.FindStringSubmatch(html)
	if len(entry) != 2 {
		return nil, fmt.Errorf("site bundle entry was not found")
	}
	appURL, err := url.Parse(entry[1])
	if err != nil {
		return nil, fmt.Errorf("parse site bundle entry: %w", err)
	}
	app, err := c.fetchBundleText(ctx, appURL.String())
	if err != nil {
		return nil, fmt.Errorf("fetch AllAnime app bundle: %w", err)
	}
	refs := bundleChunkRE.FindAllStringSubmatch(app, -1)
	seen := make(map[string]bool)
	for index, ref := range refs {
		if index >= 40 {
			break
		}
		chunkURL, err := appURL.Parse(ref[1])
		if err != nil || seen[chunkURL.String()] {
			continue
		}
		seen[chunkURL.String()] = true
		chunk, err := c.fetchBundleText(ctx, chunkURL.String())
		if err != nil || !strings.Contains(chunk, "aaReq") {
			continue
		}
		if profiles := parseBundleProfiles(chunk, c.origin); len(profiles) > 0 {
			return profiles, nil
		}
	}
	return nil, fmt.Errorf("current crypto profile was not found in site bundle")
}

func (c *Client) fetchBundleText(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	c.webHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(body), err
}

func parseBundleProfiles(js, origin string) []cryptoProfile {
	tables := make(map[string][]string)
	for _, match := range bundleTableRE.FindAllStringSubmatch(js, -1) {
		if values, ok := parseJSStringArray(match[2]); ok {
			tables[match[1]] = values
		}
	}
	decoders := make(map[string]bundleDecoder)
	for _, match := range bundleBaseRE.FindAllStringSubmatch(js, -1) {
		decoders[match[1]] = bundleDecoder{table: match[4], offset: foldJSNumber(match[3])}
	}
	if len(decoders) == 0 {
		return nil
	}
	aliases := make(map[string]bundleAlias)
	for name := range decoders {
		aliases[name] = bundleAlias{base: name}
	}
	for _, match := range bundleAliasRE.FindAllStringSubmatch(js, -1) {
		if _, ok := decoders[match[4]]; !ok {
			continue
		}
		aliases[match[1]] = bundleAlias{base: match[4], arg: boolInt(match[5] != match[2]), delta: foldJSNumber(match[6])}
	}

	var data bundleData
	for _, table := range tables {
		data.buildIDs = append(data.buildIDs, table...)
	}
	for _, match := range regexp.MustCompile(`=\[([^\]]+)\]`).FindAllStringSubmatch(js, -1) {
		items := splitTopLevel(match[1])
		if len(items) != 4 {
			continue
		}
		calls := make([][]string, len(items))
		valid := true
		for index, item := range items {
			calls[index] = bundleCallRE.FindAllString(item, -1)
			if len(calls[index]) != 2 {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		for rotation := 0; rotation < len(largestTable(tables)); rotation++ {
			seeds := make([]string, 0, len(calls))
			for _, pair := range calls {
				seeds = append(seeds, resolveBundleCall(pair[0], rotation, tables, decoders, aliases)+resolveBundleCall(pair[1], rotation, tables, decoders, aliases))
			}
			if len(seeds) == 4 && allBundleSeeds(seeds) {
				data.seeds, data.rot = seeds, rotation
				break
			}
		}
		if len(data.seeds) == 4 {
			break
		}
	}
	if len(data.seeds) != 4 {
		return nil
	}
	config := bundleConfigRE.FindStringSubmatch(js)
	if len(config) == 2 {
		data.salt[0] = configNumber(config[1], "saltMul", 117)
		data.salt[1] = configNumber(config[1], "saltAdd", 191)
		data.salt[2] = configNumber(config[1], "fragMul", 129)
		data.salt[3] = configNumber(config[1], "fragAdd", 11)
		data.boot = bundleConfigExpr(config[1], "bootPrefix", data.rot, tables, decoders, aliases)
		data.join = bundleConfigString(config[1], "join", "~")
		data.parts = bundleConfigParts(config[1], data.rot, tables, decoders, aliases)
	}
	if data.boot == "" || len(data.parts) == 0 {
		return nil
	}
	host := "mkissa.to"
	if parsed, err := url.Parse(origin); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}
	profiles := make([]cryptoProfile, 0)
	seen := make(map[string]bool)
	for _, buildID := range data.buildIDs {
		if !bundleNumberRE.MatchString(buildID) || seen[buildID] {
			continue
		}
		seen[buildID] = true
		mask, ok := deriveBundleMask(buildID, data.seeds, data.salt)
		if !ok {
			continue
		}
		profiles = append(profiles, cryptoProfile{
			BuildID: buildID, Lane: "k7", MaskHex: hex.EncodeToString(mask),
			EpochBucketMS: 604800000, GraceMS: 86400000,
			BootPrefix: data.boot, BootJoin: data.join, BootParts: data.parts,
			KeyGroup: "mkissa", Host: host,
		})
	}
	return profiles
}

func resolveBundleCall(value string, rotation int, tables map[string][]string, decoders map[string]bundleDecoder, aliases map[string]bundleAlias) string {
	match := bundleCallRE.FindStringSubmatch(value)
	if len(match) == 0 {
		return ""
	}
	alias, ok := aliases[match[1]]
	if !ok {
		return ""
	}
	decoder := decoders[alias.base]
	table := tables[decoder.table]
	if len(table) == 0 {
		return ""
	}
	argument := match[2]
	if alias.arg == 1 && match[3] != "" {
		argument = match[3]
	}
	index, err := strconv.Atoi(argument)
	if err != nil {
		return ""
	}
	index += alias.delta - decoder.offset + rotation
	index %= len(table)
	if index < 0 {
		index += len(table)
	}
	return table[index]
}

func deriveBundleMask(buildID string, seeds []string, params [4]int) ([]byte, bool) {
	mask := make([]byte, 32)
	stream := make([]byte, 32)
	for index := range stream {
		stream[index] = buildID[index%len(buildID)] ^ byte((index*params[0]+params[1])&0xff)
	}
	for seedIndex, seed := range seeds {
		bytes, err := base64.StdEncoding.DecodeString(seed)
		if err != nil || len(bytes) < 8 {
			return nil, false
		}
		for offset := 0; offset < 8; offset++ {
			index := seedIndex*8 + offset
			mask[index] = bytes[offset] ^ stream[index] ^ byte((seedIndex*params[2]+offset*params[3])&0xff)
		}
	}
	return mask, true
}

func parseJSStringArray(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, false
	}
	var result []string
	for index := 1; index < len(value)-1; {
		for index < len(value)-1 && (value[index] == ',' || value[index] == ' ' || value[index] == '\n') {
			index++
		}
		if index >= len(value)-1 {
			break
		}
		quote := value[index]
		if quote != '\'' && quote != '"' {
			return nil, false
		}
		index++
		var item strings.Builder
		for index < len(value)-1 && value[index] != quote {
			if value[index] == '\\' && index+1 < len(value)-1 {
				index++
			}
			item.WriteByte(value[index])
			index++
		}
		if index >= len(value)-1 {
			return nil, false
		}
		index++
		result = append(result, item.String())
	}
	return result, true
}

func foldJSNumber(value string) int {
	value = strings.ReplaceAll(value, " ", "")
	total, index := 0, 0
	for index < len(value) {
		sign := 1
		for index < len(value) && (value[index] == '+' || value[index] == '-') {
			if value[index] == '-' {
				sign = -sign
			}
			index++
		}
		start := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if start == index {
			return 0
		}
		factor, _ := strconv.Atoi(value[start:index])
		factor *= sign
		for index < len(value) && value[index] == '*' {
			index++
			sign = 1
			for index < len(value) && (value[index] == '+' || value[index] == '-') {
				if value[index] == '-' {
					sign = -sign
				}
				index++
			}
			start = index
			for index < len(value) && value[index] >= '0' && value[index] <= '9' {
				index++
			}
			if start == index {
				return 0
			}
			part, _ := strconv.Atoi(value[start:index])
			factor *= part * sign
		}
		total += factor
	}
	return total
}

func configNumber(config, key string, fallback int) int {
	match := regexp.MustCompile(regexp.QuoteMeta(key) + `:([0-9]+)`).FindStringSubmatch(config)
	if len(match) != 2 {
		return fallback
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return fallback
	}
	return value
}

func bundleConfigExpr(config, key string, rotation int, tables map[string][]string, decoders map[string]bundleDecoder, aliases map[string]bundleAlias) string {
	match := regexp.MustCompile(regexp.QuoteMeta(key) + `:(.*?),join:`).FindStringSubmatch(config)
	if len(match) != 2 {
		return ""
	}
	return resolveBundleExpr(match[1], rotation, tables, decoders, aliases)
}

func bundleConfigString(config, key, fallback string) string {
	match := regexp.MustCompile(regexp.QuoteMeta(key) + `:"([^"]*)"`).FindStringSubmatch(config)
	if len(match) != 2 {
		return fallback
	}
	return match[1]
}

func bundleConfigParts(config string, rotation int, tables map[string][]string, decoders map[string]bundleDecoder, aliases map[string]bundleAlias) []string {
	match := regexp.MustCompile(`parts:\[([^]]*)\]`).FindStringSubmatch(config)
	if len(match) != 2 {
		return nil
	}
	parts := make([]string, 0)
	for _, expression := range splitTopLevel(match[1]) {
		parts = append(parts, resolveBundleExpr(expression, rotation, tables, decoders, aliases))
	}
	return parts
}

func resolveBundleExpr(expression string, rotation int, tables map[string][]string, decoders map[string]bundleDecoder, aliases map[string]bundleAlias) string {
	var result strings.Builder
	for _, token := range strings.Split(strings.TrimSpace(expression), "+") {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, "\"") && strings.HasSuffix(token, "\"") {
			result.WriteString(strings.Trim(token, "\""))
			continue
		}
		if strings.HasPrefix(token, "'") && strings.HasSuffix(token, "'") {
			result.WriteString(strings.Trim(token, "'"))
			continue
		}
		result.WriteString(resolveBundleCall(token, rotation, tables, decoders, aliases))
	}
	return result.String()
}

func largestTable(tables map[string][]string) []string {
	var result []string
	for _, table := range tables {
		if len(table) > len(result) {
			result = table
		}
	}
	return result
}

func allBundleSeeds(values []string) bool {
	for _, value := range values {
		if !bundleSeedRE.MatchString(value) {
			return false
		}
	}
	return true
}

func splitTopLevel(value string) []string {
	var result []string
	start, depth := 0, 0
	for index, char := range value {
		switch char {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				result = append(result, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	return append(result, strings.TrimSpace(value[start:]))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
