package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Talking to a shard.
//
// Two calls: provisioning a team at creation, and telling the shard when its
// billing status suspends or restores it. Everything else goes straight from
// the client to the shard: the control plane is not on the chat path and must
// never become a hop on it.

var shardHTTP = &http.Client{Timeout: 20 * time.Second}

type provisioned struct {
	Slug     string   `json:"slug"`
	Hosts    []string `json:"hosts"`
	Invite   string   `json:"invite"`
	Existing bool     `json:"existing"`
}

// provision asks a shard to bring a team up. It is safe to retry: the shard
// answers an existing slug with what it already has rather than making a
// second team.
func provision(sh *Shard, slug, name, host string) (*provisioned, error) {
	body, _ := json.Marshal(map[string]any{"slug": slug, "name": name, "hosts": []string{host}})
	req, err := http.NewRequest("POST", sh.BaseURL+"/shard/tenants", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sh.Secret)
	res, err := shardHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shard %s: %w", sh.Name, err)
	}
	defer res.Body.Close()
	var out provisioned
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		return nil, fmt.Errorf("shard %s refused the team (%d)", sh.Name, res.StatusCode)
	}
	return &out, nil
}

// setShardStatus pushes a suspension (or its end) to the team's shard.
func setShardStatus(sh *Shard, slug string, suspended bool, version int64) error {
	body, _ := json.Marshal(map[string]any{"suspended": suspended, "version": version})
	req, err := http.NewRequest("POST", sh.BaseURL+"/shard/tenants/"+slug+"/status", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sh.Secret)
	res, err := shardHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("shard %s: %w", sh.Name, err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("shard %s refused the status (%d)", sh.Name, res.StatusCode)
	}
	return nil
}
