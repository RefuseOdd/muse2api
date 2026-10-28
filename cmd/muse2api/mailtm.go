package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const mailTMBase = "https://api.mail.tm"

func runMailTMOTP(args []string) int {
	fs := flag.NewFlagSet("mailtm-otp", flag.ContinueOnError)
	addr := fs.String("addr", "", "reuse an existing mail.tm address")
	password := fs.String("pass", "", "password for the existing address")
	timeout := fs.Int("timeout", 900, "polling duration in seconds")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var address, pass, token string
	var err error
	if *addr != "" && *password != "" {
		address, pass = *addr, *password
		token, err = mailLogin(address, pass)
		fmt.Println("[*] Reusing address:", address)
	} else {
		address, pass, token, err = mailNewAccount()
		if err != nil {
			fmt.Println("[!] Could not create a mail.tm inbox:", err)
			return 1
		}
		fmt.Println("[*] New address:", address)
		fmt.Println("    password:", pass, "(save it if you need to reuse the inbox)")
	}
	if err != nil {
		fmt.Println("[!] Login failed:", err)
		return 1
	}
	fmt.Println()
	fmt.Println("=== Enter this address on the muse.ai login page ===")
	fmt.Println("   ", address)
	fmt.Println()

	code := pollMailCode(token, *timeout)
	if code == "" {
		fmt.Println("[!] Timed out before a verification code arrived.")
		return 1
	}
	fmt.Println()
	fmt.Println("Verification code:", code)
	return 0
}

func mailRequest(method, path, token string, body any) (any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, mailTMBase+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func mailNewAccount() (string, string, string, error) {
	domainsRaw, err := mailRequest(http.MethodGet, "/domains", "", nil)
	if err != nil {
		return "", "", "", err
	}
	domains := listMember(domainsRaw)
	domain := ""
	for _, item := range domains {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if active, ok := m["isActive"].(bool); ok && !active {
			continue
		}
		if d, ok := m["domain"].(string); ok && d != "" {
			domain = d
			break
		}
	}
	if domain == "" {
		return "", "", "", fmt.Errorf("mail.tm returned no active domain")
	}
	user := randomAlnum(12)
	pass := randomAlnum(16)
	address := user + "@" + domain
	if _, err := mailRequest(http.MethodPost, "/accounts", "", map[string]any{"address": address, "password": pass}); err != nil {
		return "", "", "", err
	}
	token, err := mailLogin(address, pass)
	return address, pass, token, err
}

func mailLogin(address, password string) (string, error) {
	raw, err := mailRequest(http.MethodPost, "/token", "", map[string]any{"address": address, "password": password})
	if err != nil {
		return "", err
	}
	m, _ := raw.(map[string]any)
	token, _ := m["token"].(string)
	if token == "" {
		return "", fmt.Errorf("mail.tm returned no token")
	}
	return token, nil
}

func listMember(raw any) []any {
	switch v := raw.(type) {
	case []any:
		return v
	case map[string]any:
		if list, ok := v["hydra:member"].([]any); ok {
			return list
		}
	}
	return nil
}

var otpCodeRE = regexp.MustCompile(`(?:^|\D)(\d{4,8})(?:\D|$)`)

func pollMailCode(token string, timeout int) string {
	seen := map[string]bool{}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	fmt.Printf("[*] Polling the inbox for up to %d seconds...\n\n", timeout)
	lastStatus := time.Time{}
	for time.Now().Before(deadline) {
		raw, err := mailRequest(http.MethodGet, "/messages?page=1", token, nil)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		for _, item := range listMember(raw) {
			m, _ := item.(map[string]any)
			id, _ := m["id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			fullRaw, err := mailRequest(http.MethodGet, "/messages/"+id, token, nil)
			if err != nil {
				continue
			}
			full, _ := fullRaw.(map[string]any)
			from := "?"
			if fromMap, ok := full["from"].(map[string]any); ok {
				if s, ok := fromMap["address"].(string); ok {
					from = s
				}
			}
			subject, _ := full["subject"].(string)
			bodyText, _ := full["text"].(string)
			if bodyText == "" {
				bodyText, _ = full["intro"].(string)
			}
			fmt.Println("[+] New message from", from)
			fmt.Println("    subject:", subject)
			preview := strings.TrimSpace(bodyText)
			if len(preview) > 500 {
				preview = preview[:500]
			}
			fmt.Println("    body:", preview)
			if match := otpCodeRE.FindStringSubmatch(subject + "\n" + bodyText); len(match) > 1 {
				return match[1]
			}
			fmt.Println("    (no verification code found yet)")
			fmt.Println()
		}
		if time.Since(lastStatus) > 15*time.Second {
			lastStatus = time.Now()
			fmt.Printf("    waiting... %ds left\n", int(time.Until(deadline).Seconds()))
		}
		time.Sleep(3 * time.Second)
	}
	return ""
}

func randomAlnum(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}
