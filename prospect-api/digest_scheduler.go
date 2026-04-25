package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// startDigestScheduler starts a goroutine that fires POST /api/morning-digest
// once per day at the configured local hour (default 06:00 in PROSPECT_TIMEZONE).
//
// Uses time.NewTimer resetting every 24h so it drifts at most one tick per day
// rather than accumulating drift from time.NewTicker. The endpoint is called
// via internal HTTP so the same auth + audit path runs as for external callers.
func startDigestScheduler(cfg *Config, bearerToken string) {
	loc, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		log.Printf("digest_scheduler: load tz %q: %v — scheduler disabled", cfg.TimeZone, err)
		return
	}

	go func() {
		for {
			now := time.Now().In(loc)
			// Next 06:00 local.
			next := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, loc)
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			wait := next.Sub(now)
			log.Printf("digest_scheduler: next fire in %s (at %s local)", wait.Round(time.Minute), next.Format("2006-01-02 15:04"))

			timer := time.NewTimer(wait)
			<-timer.C
			timer.Stop()

			fireDigest(cfg, bearerToken)
		}
	}()
}

// fireDigest calls POST /api/morning-digest on the loopback server.
func fireDigest(cfg *Config, bearerToken string) {
	url := fmt.Sprintf("http://%s:%d/api/morning-digest", cfg.BindHost, cfg.BindPort)
	body := bytes.NewBufferString(`{}`)
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		log.Printf("digest_scheduler: build request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("digest_scheduler: fire: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		log.Printf("digest_scheduler: endpoint returned %d: %s", resp.StatusCode, string(respBody))
		return
	}
	log.Printf("digest_scheduler: fired successfully: %s", string(respBody))
}
