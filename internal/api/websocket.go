package api

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/keneo/docker-dynamic-limits/internal/events"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		http.Error(w, "events not available", http.StatusServiceUnavailable)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Parse filters from query params
	var filter events.Filter
	if ids := r.URL.Query().Get("container_id"); ids != "" {
		filter.ContainerIDs = strings.Split(ids, ",")
	}
	if types := r.URL.Query().Get("types"); types != "" {
		for _, t := range strings.Split(types, ",") {
			filter.Types = append(filter.Types, events.EventType(t))
		}
	}

	sub := s.bus.Subscribe(filter)

	// Read goroutine: drain client messages and detect close
	closeCh := make(chan struct{})
	go func() {
		defer close(closeCh)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Write loop: send events until client disconnects or server shuts down
	defer s.bus.Unsubscribe(sub)
	defer conn.Close()

	for {
		select {
		case evt, ok := <-sub.C:
			if !ok {
				// Bus unsubscribed us (e.g. during shutdown)
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown"))
				return
			}
			if err := conn.WriteJSON(evt); err != nil {
				return
			}
		case <-closeCh:
			return
		case <-s.done:
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown"))
			return
		}
	}
}
