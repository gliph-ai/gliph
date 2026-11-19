package main

import "time"

// presenceDelay is how long arrivals and departures are gathered before the
// member lists that show them are refreshed.
//
// A refresh is a full state frame per online member, and #general holds
// everyone. Sent per arrival, a reconnect storm after a deploy cost
// arrivals × online members × members state work: 500 clients took minutes
// to get back in on two cores. Gathered, each online member gets one frame
// per window however many people came or went in it.
//
// ponytail: still full state frames, just fewer. A presence delta frame
// (wire + core change) if the per-window cost ever shows up.
const presenceDelay = 500 * time.Millisecond

// pushPresence refreshes the member lists that show this user, so the rail
// marks them online or offline. Per user, not per socket: a second tab is
// not an arrival.
func (s *Server) pushPresence(uid string) {
	chans := s.userChannels(uid)
	s.presMu.Lock()
	defer s.presMu.Unlock()
	if s.presDirty == nil {
		s.presDirty = map[string]bool{}
		time.AfterFunc(presenceDelay, s.flushPresence)
	}
	for _, ch := range chans {
		s.presDirty[ch.ID] = true
	}
}

// flushPresence sends one state frame to each online member of every channel
// that saw an arrival or departure since the last flush.
func (s *Server) flushPresence() {
	s.presMu.Lock()
	dirty := s.presDirty
	s.presDirty = nil
	s.presMu.Unlock()
	seen := map[string]bool{}
	for cid := range dirty {
		for _, uid := range s.St.MemberIDs(cid) {
			if !seen[uid] {
				seen[uid] = true
				s.PushState(uid) // skips anyone without a socket
			}
		}
	}
}
