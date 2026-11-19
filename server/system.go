package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
)

// System messages: the two lines in the stream that are not chat.
//
// A MOTD is the operator's, set in the admin console and stored in `settings`.
// Release notes are ours, written per feature and compiled into the binary —
// they describe the build the reader is running, so shipping them with it is
// what keeps the two in step.
//
// Both ride in `hello`. Which of them a client has already seen is the
// client's business (core/src/client.rs), so the server holds no per-user
// read state for either.

//go:embed releases.json
var releasesJSON []byte

type Release struct {
	ID    string `json:"id"`
	Date  string `json:"date"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// How many of the newest notes ride in hello. A client that has been away for
// six releases gets the last five; the whole list is at GET /api/releases.
const newsInHello = 5

var releases = loadReleases()

func loadReleases() []Release {
	var rs []Release
	if err := json.Unmarshal(releasesJSON, &rs); err != nil {
		log.Printf("releases.json: %v", err)
		return nil
	}
	// The file is appended to by hand, so its order is not the order to read
	// them in. Dates are ISO, so a string sort is a date sort.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Date < rs[j].Date })
	return rs
}

// news is what hello carries: the newest few, oldest first.
func news() []Release {
	if len(releases) > newsInHello {
		return releases[len(releases)-newsInHello:]
	}
	return releases
}

const (
	settingMOTD = "motd"
	settingIcon = "motd_icon"
)

// motd is what hello carries: the text, the org icon as a data URL, and an id
// that changes when the text does. The id is what a client dismisses, so
// editing the MOTD shows it to everyone again and re-saving the same words
// does not.
func (s *Server) motd() map[string]any {
	text := strings.TrimSpace(s.St.Setting(settingMOTD))
	if text == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(text))
	return map[string]any{
		"id":   "motd-" + hex.EncodeToString(sum[:6]),
		"text": text,
		"icon": s.St.Setting(settingIcon),
	}
}

// A saved MOTD reaches everyone who is already connected. Without this the
// operator's announcement waits for each client's next reconnect, which for a
// desktop app that has been open since Tuesday is not a useful moment.
func (s *Server) BroadcastMOTD() {
	s.Broadcast(Frame{T: "motd", D: map[string]any{"motd": s.motd()}})
}

// maxIconBytes caps the org icon. It is a data URL sent inside every hello, so
// it is small on purpose: an icon is drawn at 28 pixels.
const maxIconBytes = 96 * 1024

// validIcon accepts "" (clear the icon) or a small inline image. Nothing else:
// this string is handed to every client as an <img src>, and the CSP that
// keeps a chat client from loading a stranger's URL only holds if what we put
// there is inline bytes.
func validIcon(v string) bool {
	if v == "" {
		return true
	}
	return len(v) <= maxIconBytes && strings.HasPrefix(v, "data:image/") && strings.Contains(v, ";base64,")
}
