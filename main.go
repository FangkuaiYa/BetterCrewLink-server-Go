package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type GameState int

const (
	LOBBY GameState = iota
	TASKS
	DISCUSSION
	MENU
	UNKNOWN
)

func (g GameState) String() string {
	return [...]string{"LOBBY", "TASKS", "DISCUSSION", "MENU", "UNKNOWN"}[g]
}

type PublicLobby struct {
	ID             int       `json:"id"`
	Title          string    `json:"title"`
	Host           string    `json:"host"`
	CurrentPlayers int       `json:"current_players"`
	MaxPlayers     int       `json:"max_players"`
	Language       string    `json:"language"`
	Mods           string    `json:"mods"`
	IsPublic       bool      `json:"isPublic"`
	IsPublic2      bool      `json:"isPublic2,omitempty"`
	Server         string    `json:"server"`
	GameState      GameState `json:"gameState"`
	StateTime      int64     `json:"stateTime"`
}

type ClientInfo struct {
	PlayerId  int    `json:"playerId"`
	ClientId  int    `json:"clientId"`
	SocketID  string `json:"-"`
	LobbyCode string `json:"-"`
}

type Signal struct {
	Data string `json:"data"`
	To   string `json:"to"`
}

type PeerConfig struct {
	ForceRelayOnly bool
}

var (
	clients      = make(map[string]*ClientInfo)
	publicLobbies = make(map[string]PublicLobby)
	lobbyCodes   = make(map[int]string)
	allLobbies   = make(map[string]int)
	socketRooms  = make(map[string]string)
	rooms        = make(map[string]map[string]*wsConn)
	lobbyCount   = 0
	connectionCount = 0
	mu           sync.RWMutex
	peerConfig   = PeerConfig{ForceRelayOnly: false}
	hostname     string
	startTime    = time.Now()
)

type wsConn struct {
	conn   net.Conn
	id     string
	send   chan []byte
}

func generateSocketID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(i)
	}
	return fmt.Sprintf("%x", b)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrade(w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	id := generateSocketID()
	client := &wsConn{
		conn: conn,
		id:   id,
		send: make(chan []byte, 256),
	}

	mu.Lock()
	clients[id] = &ClientInfo{SocketID: id}
	mu.Unlock()
	connectionCount++
	log.Printf("Client connected: %s, total: %d", id, connectionCount)

	go client.readPump()
	go client.writePump()

	config := map[string]interface{}{
		"forceRelayOnly": peerConfig.ForceRelayOnly,
		"iceServers":     []map[string]string{{"urls": "stun:stun.l.google.com:19302"}},
	}
	if hostname != "" {
		config["iceServers"] = append(config["iceServers"].([]map[string]string), map[string]string{
			"urls":       fmt.Sprintf("turn:%s:3478", hostname),
			"username":   "M9DRVaByiujoXeuYAAAG",
			"credential": "TpHR9HQNZ8taxjb3",
		})
	}
	data, _ := json.Marshal(map[string]interface{}{"type": "clientPeerConfig", "data": config})
	client.send <- data

	reader := bufio.NewReader(conn)
	for {
		frameType, payload, err := readWebSocketFrame(reader)
		if err != nil {
			break
		}
		if frameType == 0x8 {
			break
		}
		if frameType != 0x1 {
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		eventType := fmt.Sprintf("%v", msg["type"])
		dispatchEvent(client, eventType, msg)
	}

	cleanupClient(client)
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	magic := "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	hash := sha1.Sum([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(hash[:])

	w.Header().Set("Upgrade", "websocket")
	w.Header().Set("Connection", "Upgrade")
	w.Header().Set("Sec-WebSocket-Accept", accept)
	w.Header().Set("Sec-WebSocket-Version", "13")
	w.WriteHeader(http.StatusSwitchingProtocols)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijacking not supported")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *wsConn) readPump() {
	// handled in main goroutine
}

func (c *wsConn) writePump() {
	defer c.conn.Close()
	for {
		select {
		case message := <-c.send:
			writeWebSocketFrame(c.conn, 0x1, message)
		}
	}
}

func dispatchEvent(conn *wsConn, event string, msg map[string]interface{}) {
	switch event {
	case "clientPeerConfig":
		// Already sent during connection
	case "join":
		handleJoin(conn, msg)
	case "setHost":
		handleSetHost(conn, msg)
	case "id":
		handleId(conn, msg)
	case "leave":
		handleLeave(conn)
	case "VAD":
		handleVAD(conn, msg)
	case "join_lobby":
		handleJoinLobby(conn, msg)
	case "lobby":
		handleLobby(conn, msg)
	case "remove_lobby":
		handleRemoveLobby(conn, msg)
	case "signal":
		handleSignal(conn, msg)
	case "lobbybrowser":
		handleLobbyBrowser(conn, msg)
	}
}

func cleanupClient(client *wsConn) {
	mu.Lock()
	code := socketRooms[client.id]
	if code != "" {
		if r, ok := rooms[code]; ok {
			delete(r, client.id)
			if len(r) == 0 {
				delete(rooms, code)
				delete(allLobbies, code)
				removePublicLobby(code)
			} else {
				_, ok := allLobbies[code]
				if ok {
					allLobbies[code]--
				}
			}
		}
	}
	delete(clients, client.id)
	delete(socketRooms, client.id)
	connectionCount--
	mu.Unlock()
	log.Printf("Client disconnected: %s, total: %d", client.id, connectionCount)
}

func handleJoin(conn *wsConn, msg map[string]interface{}) {
	c := fmt.Sprintf("%v", msg["c"])
	id := int(msg["id"].(float64))
	clientId := int(msg["clientId"].(float64))
	if c == "" {
		return
	}

	otherClients := make(map[string]ClientInfo)
	mu.RLock()
	for sid, cl := range clients {
		if sid != conn.id && socketRooms[sid] == c {
			otherClients[sid] = *cl
		}
	}
	mu.RUnlock()

	mu.Lock()
	if _, exists := allLobbies[c]; !exists {
		allLobbies[c] = 1
	} else {
		allLobbies[c]++
	}
	socketRooms[conn.id] = c
	clients[conn.id] = &ClientInfo{PlayerId: id, ClientId: clientId, SocketID: conn.id, LobbyCode: c}
	mu.Unlock()

	if _, ok := rooms[c]; !ok {
		rooms[c] = make(map[string]*wsConn)
	}
	rooms[c][conn.id] = conn

	emitToSocket(conn, "setClients", otherClients)
	broadcastToRoom(c, "join", map[string]interface{}{"playerId": id, "clientId": clientId, "socketId": conn.id})
	emitToSocket(conn, "setHost", getHostId(c))
}

func handleSetHost(conn *wsConn, msg map[string]interface{}) {
	c := fmt.Sprintf("%v", msg["c"])
	clientId := int(msg["clientId"].(float64))
	broadcastToRoom(c, "setHost", clientId)
}

func handleId(conn *wsConn, msg map[string]interface{}) {
	id := int(msg["id"].(float64))
	clientId := int(msg["clientId"].(float64))
	clients[conn.id] = &ClientInfo{PlayerId: id, ClientId: clientId, SocketID: conn.id, LobbyCode: socketRooms[conn.id]}
	code := socketRooms[conn.id]
	broadcastToRoom(code, "setClient", conn.id, map[string]interface{}{"playerId": id, "clientId": clientId})
}

func handleLeave(conn *wsConn) {
	code := socketRooms[conn.id]
	if code == "" {
		return
	}
	mu.Lock()
	if r, ok := rooms[code]; ok {
		delete(r, conn.id)
		if len(r) == 0 {
			delete(rooms, code)
			delete(allLobbies, code)
			removePublicLobby(code)
		} else {
			_, ok := allLobbies[code]
			if ok {
				allLobbies[code]--
			}
		}
	}
	delete(clients, conn.id)
	delete(socketRooms, conn.id)
	mu.Unlock()
}

func handleVAD(conn *wsConn, msg map[string]interface{}) {
	activity := msg["activity"].(bool)
	code := socketRooms[conn.id]
	if code == "" {
		return
	}
	broadcastToRoom(code, "VAD", map[string]interface{}{"activity": activity, "socketId": conn.id})
}

func handleJoinLobby(conn *wsConn, msg map[string]interface{}) {
	id := int(msg["id"].(float64))
	mu.RLock()
	if code, exists := lobbyCodes[id]; exists {
		if pl, pubExists := publicLobbies[code]; pubExists && pl.IsPublic && pl.GameState == LOBBY {
			mu.RUnlock()
			emitToSocket(conn, "join_lobby_callback", []interface{}{0, code, pl.Server, pl})
			return
		}
	}
	mu.RUnlock()
	emitToSocket(conn, "join_lobby_callback", []interface{}{1, "Lobby not found"})
}

func handleLobby(conn *wsConn, msg map[string]interface{}) {
	c := fmt.Sprintf("%v", msg["c"])
	lobbyData, ok := msg["lobby"].(map[string]interface{})
	if !ok {
		return
	}
	if _, exists := allLobbies[c]; !exists {
		return
	}

	title := getString(lobbyData, "title", "ERROR")
	host := getString(lobbyData, "host", "")
	currentPlayers := getInt(lobbyData, "current_players", 0)
	maxPlayers := getInt(lobbyData, "max_players", 0)
	language := getString(lobbyData, "language", "")
	mods := getString(lobbyData, "mods", "")
	isPublic := getBool(lobbyData, "isPublic", false)
	isPublic2 := getBool(lobbyData, "isPublic2", false)
	server := getString(lobbyData, "server", "")
	gameState := parseGameState(fmt.Sprintf("%v", lobbyData["gameState"]))

	mu.Lock()
	publobby, exists := publicLobbies[c]
	id := lobbyCount
	if exists {
		id = publobby.ID
	}
	stateTime := int64(0)
	if exists {
		sameState := (publobby.GameState == LOBBY && gameState == LOBBY) ||
			(publobby.GameState != LOBBY && gameState != LOBBY)
		if sameState {
			stateTime = publobby.StateTime
		} else {
			stateTime = time.Now().UnixMilli()
		}
	} else {
		lobbyCount++
		stateTime = time.Now().UnixMilli()
	}
	pubLobby := PublicLobby{
		ID:             id,
		Title:          truncate(title, 20),
		Host:           truncate(host, 10),
		CurrentPlayers: currentPlayers,
		MaxPlayers:     maxPlayers,
		Language:       truncate(language, 5),
		Mods:           strings.ToUpper(truncate(mods, 20)),
		IsPublic:       isPublic || isPublic2,
		Server:         server,
		GameState:      gameState,
		StateTime:      stateTime,
	}
	lobbyCodes[id] = c
	publicLobbies[c] = pubLobby
	mu.Unlock()

	broadcastToRoom("lobbybrowser", "update_lobby", pubLobby)
}

func handleRemoveLobby(conn *wsConn, msg map[string]interface{}) {
	c := fmt.Sprintf("%v", msg["c"])
	removePublicLobby(c)
}

func handleSignal(conn *wsConn, msg map[string]interface{}) {
	data := fmt.Sprintf("%v", msg["data"])
	to := fmt.Sprintf("%v", msg["to"])
	if data == "" || to == "" {
		return
	}
	mu.RLock()
	targetRoom := socketRooms[to]
	mu.RUnlock()
	if targetRoom == "" {
		return
	}
	mu.RLock()
	room := rooms[targetRoom]
	mu.RUnlock()
	if room == nil {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{"type": "signal", "data": map[string]interface{}{"data": data, "from": conn.id}})
	mu.RLock()
	for _, c := range room {
		select {
		case c.send <- payload:
		default:
		}
	}
	mu.RUnlock()
}

func handleLobbyBrowser(conn *wsConn, msg map[string]interface{}) {
	open := msg["open"].(bool)
	if !open {
		mu.Lock()
		if r, ok := rooms["lobbybrowser"]; ok {
			delete(r, conn.id)
		}
		mu.Unlock()
		return
	}
	mu.Lock()
	if _, ok := rooms["lobbybrowser"]; !ok {
		rooms["lobbybrowser"] = make(map[string]*wsConn)
	}
	rooms["lobbybrowser"][conn.id] = conn
	mu.Unlock()
	mu.RLock()
	lobbies := make([]PublicLobby, 0, len(publicLobbies))
	for _, l := range publicLobbies {
		lobbies = append(lobbies, l)
	}
	mu.RUnlock()
	broadcastToRoom("lobbybrowser", "new_lobbies", lobbies)
}

func removePublicLobby(code string) {
	mu.Lock()
	defer mu.Unlock()
	if pl, exists := publicLobbies[code]; exists {
		broadcastToRoom("lobbybrowser", "remove_lobby", pl.ID)
		delete(lobbyCodes, pl.ID)
		delete(publicLobbies, code)
	}
}

func getHostId(code string) int {
	mu.RLock()
	defer mu.RUnlock()
	if l, ok := allLobbies[code]; ok {
		return l
	}
	return -1
}

func getString(m map[string]interface{}, key string, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getInt(m map[string]interface{}, key string, def int) int {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case float64:
			return int(val)
		case int:
			return val
		}
	}
	return def
}

func getBool(m map[string]interface{}, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func parseGameState(s string) GameState {
	switch s {
	case "LOBBY":
		return LOBBY
	case "TASKS":
		return TASKS
	case "DISCUSSION":
		return DISCUSSION
	case "MENU":
		return MENU
	}
	return UNKNOWN
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func broadcastToRoom(code string, event string, data ...interface{}) {
	mu.RLock()
	room := rooms[code]
	mu.RUnlock()
	if room == nil {
		return
	}
	var payload []byte
	if len(data) == 1 {
		payload, _ = json.Marshal(map[string]interface{}{"type": event, "data": data[0]})
	} else {
		payload, _ = json.Marshal(map[string]interface{}{"type": event, "data": data})
	}
	mu.RLock()
	for _, conn := range room {
		select {
		case conn.send <- payload:
		default:
		}
	}
	mu.RUnlock()
}

func emitToSocket(conn *wsConn, event string, data interface{}) {
	payload, _ := json.Marshal(map[string]interface{}{"type": event, "data": data})
	select {
	case conn.send <- payload:
	default:
	}
}

func readWebSocketFrame(r *bufio.Reader) (byte, []byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	second, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := first & 0x0F
	masked := (second & 0x80) != 0
	payloadLen := int(second & 0x7F)

	var maskingKey [4]byte
	var extendedPayloadLen uint64

	switch payloadLen {
	case 126:
		b1, _ := r.ReadByte()
		b2, _ := r.ReadByte()
		payloadLen = int(b1)<<8 | int(b2)
	case 127:
		for i := 0; i < 8; i++ {
			b, _ := r.ReadByte()
			extendedPayloadLen = extendedPayloadLen<<8 | uint64(b)
		}
		payloadLen = int(extendedPayloadLen)
	}

	if masked {
		r.Read(maskingKey[:])
	}

	payload := make([]byte, payloadLen)
	io.ReadFull(r, payload)
	if masked {
		for i := 0; i < payloadLen; i++ {
			payload[i] ^= maskingKey[i%4]
		}
	}

	return opcode, payload, nil
}

func writeWebSocketFrame(w io.Writer, opcode byte, payload []byte) {
	var header [10]byte
	header[0] = 0x80 | opcode
	if len(payload) < 126 {
		header[1] = byte(len(payload))
		w.Write(header[:2])
	} else if len(payload) < 65536 {
		header[1] = 126
		header[2] = byte(len(payload) >> 8)
		header[3] = byte(len(payload))
		w.Write(header[:4])
	} else {
		header[1] = 127
		for i := 7; i >= 0; i-- {
			header[2+i] = byte(len(payload) >> (8 * i))
		}
		w.Write(header[:10])
	}
	w.Write(payload)
}

func getServerAddress(r *http.Request) string {
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = r.Header.Get("X-Forwarded-Proto")
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	address := getServerAddress(r)
	html := getHomeHTML(address)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	fmt.Fprint(w, html)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	address := getServerAddress(r)
	mu.RLock()
	lc := len(allLobbies)
	cc := connectionCount
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"uptime":          int(time.Since(startTime).Seconds()),
		"connectionCount": cc,
		"lobbiesCount":    lc,
		"address":         address,
		"name":            os.Getenv("NAME"),
	})
}

func lobbiesHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	lobbies := make([]PublicLobby, 0, len(publicLobbies))
	for _, l := range publicLobbies {
		lobbies = append(lobbies, l)
	}
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lobbies)
}

func getHomeHTML(address string) string {
	safeAddr := strings.ReplaceAll(address, `"`, `\"`)
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<title>BetterCrewLink Server</title>
<link rel="stylesheet" href="/public/styles.css">
<link rel="icon" type="image/x-icon" href="/public/icon.ico">
</head>
<body>
<canvas id="stars"></canvas>
<div class="container">
  <img class="logo" src="/public/logo-big.png" alt="BetterCrewLink">
  <div class="card">
    <div class="stat">
      <span class="label">Server Address</span>
      <span class="value value--url" id="address">` + safeAddr + `</span>
    </div>
    <div class="stat">
      <span class="label">Connected Users</span>
      <span class="value" id="connections">0</span>
    </div>
    <div class="stat">
      <span class="label">Active Lobbies</span>
      <span class="value" id="lobbies">0</span>
    </div>
  </div>
  <div class="links">
    <a href="https://github.com/OhMyGuus/BetterCrewLink/releases" target="_blank" rel="noopener noreferrer">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
      Download
    </a>
    <a href="https://web.bettercrewl.ink" target="_blank" rel="noopener noreferrer">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
      Web Version
    </a>
  </div>
</div>
<script src="/public/stars.js"></script>
<script src="/public/landing.js"></script>
</body>
</html>`
}

func main() {
	hostname = os.Getenv("HOSTNAME")
	port := os.Getenv("PORT")
	if port == "" {
		port = "9736"
	}

	router := http.NewServeMux()
	router.HandleFunc("/", homeHandler)
	router.HandleFunc("/health", healthHandler)
	router.HandleFunc("/lobbies", lobbiesHandler)
	router.HandleFunc("/socket.io", handleWebSocket)
	router.Handle("/public/", http.StripPrefix("/public/", http.FileServer(http.Dir("public"))))

	addr := ":" + port
	log.Printf("BetterCrewLink Server started on %s", addr)
	log.Fatal(http.ListenAndServe(addr, router))
}
