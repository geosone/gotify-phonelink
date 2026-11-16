package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sendSMS sends a text through smstools-http-api
// (POST {url}/api/v1.0/sms/outgoing, basic auth) and checks the per-number
// answer "Ok". phone is E.164; smstools expects the digits without "+".
func sendSMS(ctx context.Context, client *http.Client, cfg *SMSConfig, phone, text string) error {
	number := strings.TrimPrefix(phone, "+")
	body := map[string]interface{}{
		"mobiles": []string{number},
		"text":    text,
	}
	if cfg.Queue != "" {
		body["queue"] = cfg.Queue
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(cfg.URL, "/")+"/api/v1.0/sms/outgoing", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.Username != "" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sms api: %w", err)
	}
	defer res.Body.Close()
	resBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("sms api rejected the credentials (HTTP %d)", res.StatusCode)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("sms api: HTTP %d", res.StatusCode)
	}
	var result struct {
		Mobiles map[string]struct {
			Response string `json:"response"`
		} `json:"mobiles"`
	}
	if err := json.Unmarshal(resBody, &result); err != nil {
		return fmt.Errorf("sms api: %w", err)
	}
	r, ok := result.Mobiles[number]
	if !ok {
		return fmt.Errorf("sms api: no result for %s", maskPhone(phone))
	}
	if !strings.EqualFold(r.Response, "Ok") {
		return fmt.Errorf("sms api refused the message: %s", r.Response)
	}
	return nil
}
