package engine

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// These indirections exist so safety behavior can be tested without network.
var (
	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]net.IP, 0, len(addresses))
		for _, address := range addresses {
			out = append(out, address.IP)
		}
		return out, nil
	}
	refDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
)

// NormalizeImage validates one reference image and returns canonical base64
// plus a MIME type. Default policy allows data URLs, raw base64 and public
// URLs; server-local paths and private network fetches require explicit
// opt-ins.
func (e *Engine) NormalizeImage(image string) (string, string, error) {
	return e.NormalizeImageContext(context.Background(), image)
}
func (e *Engine) NormalizeImageContext(ctx context.Context, image string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return "", "image/png", nil
	}
	checked := func(data []byte, mimeType string) (string, string, error) {
		mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
		if !strings.HasPrefix(mimeType, "image/") {
			return "", "", genError("reference file is not an image (%s)", mimeType)
		}
		if len(data) > MaxReferenceImageBytes {
			return "", "", genError("single reference image exceeds the 10 MiB limit")
		}
		return base64.StdEncoding.EncodeToString(data), mimeType, nil
	}

	if strings.HasPrefix(image, "data:") {
		head, payload, ok := strings.Cut(image, ",")
		if !ok {
			return "", "", genError("reference image Data URL is malformed")
		}
		mimeType := "image/png"
		if semi := strings.Index(head, ";"); semi >= 0 {
			if head[5:semi] != "" {
				mimeType = head[5:semi]
			}
		} else if len(head) > 5 {
			mimeType = head[5:]
		}
		var data []byte
		var err error
		if strings.Contains(strings.ToLower(head), ";base64") {
			data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
		} else {
			var decoded string
			decoded, err = url.PathUnescape(payload)
			data = []byte(decoded)
		}
		if err != nil {
			return "", "", genError("reference image Data URL has invalid encoding")
		}
		return checked(data, mimeType)
	}

	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		resp, err := e.openReferenceContext(ctx, image)
		if err != nil {
			if IsSafetyError(err) {
				return "", "", err
			}
			return "", "", genError("download remote reference image failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", "", genError("reference image HTTP %d", resp.StatusCode)
		}
		mimeType := resp.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "image/png"
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxReferenceImageBytes+1))
		if rerr != nil {
			return "", "", genError("download remote reference image failed: %v", rerr)
		}
		return checked(data, mimeType)
	}

	if data, err := base64.StdEncoding.DecodeString(image); err == nil && len(data) > 0 {
		return checked(data, "image/png")
	}

	if !e.cfg.RefImageLocalFiles {
		return "", "", safetyError("reference image local paths are denied; set MUSE2API_REF_IMAGE_LOCAL_FILES=1 to allow server files")
	}
	st, err := os.Stat(image)
	if err == nil && st.Mode().IsRegular() {
		if st.Size() > MaxReferenceImageBytes {
			return "", "", genError("single reference image exceeds the 10 MiB limit")
		}
		data, rerr := os.ReadFile(image)
		if rerr != nil {
			return "", "", genError("failed to read local reference image: %v", rerr)
		}
		mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(image)))
		if mimeType == "" {
			mimeType = "image/png"
		}
		return checked(data, mimeType)
	}
	return "", "", genError("reference image must be a URL, data URL, local image, or valid base64")
}

func (e *Engine) openReferenceURL(rawURL string) (*http.Response, error) {
	return e.openReferenceContext(context.Background(), rawURL)
}
func (e *Engine) openReferenceContext(ctx context.Context, rawURL string) (*http.Response, error) {
	if e.cfg.RefImagePrivateFetch {
		client := &http.Client{Timeout: 20 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		return client.Do(req)
	}

	current := rawURL
	for attempt := 0; attempt < 11; attempt++ {
		parsed, err := url.Parse(current)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return nil, safetyError("reference image redirects must use a valid http(s) URL")
		}
		port := parsed.Port()
		if port == "" {
			if parsed.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		ips, err := refLookupIP(ctx, parsed.Hostname())
		if err != nil {
			return nil, err
		}
		checked := make([]string, 0, len(ips))
		for _, rawIP := range ips {
			ip := unmapIP(rawIP)
			if !isGlobalIP(ip) {
				return nil, safetyError("reference image address denied: only public addresses are allowed; set MUSE2API_REF_IMAGE_PRIVATE_FETCH=1 to allow private fetches")
			}
			checked = append(checked, ip.String())
		}
		if len(checked) == 0 {
			return nil, fmt.Errorf("reference image host did not resolve to an address")
		}
		pinned := checked[0]
		serverName := parsed.Hostname()
		transport := &http.Transport{
			Proxy:                 nil,
			DisableKeepAlives:     true,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   20 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			TLSClientConfig:       &tls.Config{ServerName: serverName},
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				_, targetPort, err := net.SplitHostPort(address)
				if err != nil {
					targetPort = port
				}
				return refDialContext(ctx, network, net.JoinHostPort(pinned, targetPort))
			},
		}
		client := &http.Client{
			Transport: transport,
			Timeout:   20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if isRedirect(resp.StatusCode) {
			location := resp.Header.Get("Location")
			if location == "" {
				return resp, nil
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			ref, err := url.Parse(location)
			if err != nil {
				return nil, err
			}
			current = parsed.ResolveReference(ref).String()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("too many reference image redirects")
}

func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func unmapIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

var nonGlobalIPv4 = mustCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

var nonGlobalIPv6 = mustCIDRs(
	"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	"2001:db8::/32", "2001:10::/28", "2001:20::/28", "2001:2::/48",
	"3fff::/20", "5f00::/16",
)

var globalIPv6Block = mustCIDRs("2000::/3")

func mustCIDRs(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		if _, block, err := net.ParseCIDR(value); err == nil {
			out = append(out, block)
		}
	}
	return out
}

func isGlobalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip = unmapIP(ip)
	if v4 := ip.To4(); v4 != nil {
		for _, block := range nonGlobalIPv4 {
			if block.Contains(v4) {
				return false
			}
		}
		return true
	}
	v6 := ip.To16()
	if v6 == nil || !v6.IsGlobalUnicast() {
		return false
	}
	inGlobal := false
	for _, block := range globalIPv6Block {
		if block.Contains(v6) {
			inGlobal = true
			break
		}
	}
	if !inGlobal {
		return false
	}
	for _, block := range nonGlobalIPv6 {
		if block.Contains(v6) {
			return false
		}
	}
	return true
}
