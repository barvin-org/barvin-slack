package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"github.com/gorilla/websocket"
	"github.com/op/go-logging"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type slackEvent struct {
	Type      string `json:"type"`
	User      string `json:"user"`
	Text      string `json:"text"`
	Timestamp string `json:"ts"`
}

type restart struct {
	Reason string
}

type client struct {
	conn net.Conn
	name string
}

type connToUserMsg struct {
	UserID  string
	Message string
}

type connToAllUsersMsg struct {
	Message string
}

type connReplyMsg struct {
	Message string
}

type user struct {
	ID   string
	Recv chan slackEvent
	Send chan connReplyMsg
}

type connRegister struct {
	Name string
	Conn net.Conn
}

type connUnregister struct {
	Name string
}

type connMessage struct {
	Type   string
	Msg    string
	UserID string
}

type msg struct {
	Type string `json:"type"`
	Data string `json:"data"`
	User string `json:"user,omitempty"`
}

type config struct {
	Token string   `json:"token"`
	Users []string `json:"users"`
	Port  int      `json:"port"`
	Host  string   `json:"host"`
}

type rtmResponse struct {
	URL string `json:"url"`
}

type ctx struct {
	Restarts    chan restart
	SlackEvents chan string
	UserEvents  chan interface{}
	Connections chan interface{}
}

var log = logging.MustGetLogger("main-logger")

func readConfig() config {
	config := config{
		Token: "",
		Users: []string{},
		Port:  9191,
		Host:  "localhost",
	}
	file, errReadFile := ioutil.ReadFile("config.json")
	if errReadFile != nil {
		log.Warningf("Config file error: %s; using default values.", errReadFile)
		return config
	}
	errUnmarshal := json.Unmarshal(file, &config)
	if errUnmarshal != nil {
		log.Warningf("Config file error: %s; using default values.", errUnmarshal)
		return config
	}
	return config
}

func startRtm(origin string) rtmResponse {
	resp, err := http.Get(origin)
	if err != nil {
		return rtmResponse{}
	}
	defer resp.Body.Close()
	var data rtmResponse
	json.NewDecoder(resp.Body).Decode(&data)
	return data
}

func connectWs(url string, origin string) (*websocket.Conn, error) {
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	return c, err
}

func eventsProducer(ws *websocket.Conn, ctx ctx) {
	ticker := time.NewTicker(10 * time.Second)
	stopPinger := make(chan struct{})
	ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(15 * time.Second))
		return nil
	})
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					log.Errorf("Error while writing ping: %v", err)
				}
			case <-stopPinger:
				ticker.Stop()
				return
			}
		}
	}()
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Errorf("Error while reading message: %v", err)
			close(stopPinger)
			ws.Close()
			ctx.Restarts <- restart{Reason: "slack read message error"}
			go func() {
				ctx.Connections <- connMessage{Type: "restart", Msg: "slack read message error"}
			}()
			break
		}
		ctx.SlackEvents <- string(msg[:])
	}
}

func parseSlackEvent(jsonEvent string) (slackEvent, error) {
	var event slackEvent
	err := json.Unmarshal([]byte(jsonEvent), &event)
	if err != nil {
		return event, errors.New("Invalid event")
	}
	return event, nil
}

func dispatchSlackEvents(ctx ctx) {
	for {
		jsonEvent := <-ctx.SlackEvents
		event, _ := parseSlackEvent(jsonEvent)
		go func() {
			ctx.UserEvents <- event
		}()
	}
}

func userReceiveMessages(ctx ctx, u user) {
	for {
		m := <-u.Recv
		if m.Type == "message" {
			go func() {
				ctx.Connections <- connMessage{
					Type:   "msg",
					Msg:    m.Text,
					UserID: u.ID,
				}
			}()
		}
		log.Infof("Dispatching event: %#v, for user: %#v", m, u)
	}
}

func userSendMessages(ctx ctx, u user) {
	for {
		m := <-u.Send
		log.Infof("Message for the user: %#v, for user: %s", m, u.ID)
	}
}

func usersRegistrar(ctx ctx, userIds []string) {
	users := make(map[string]user)
	for _, userID := range userIds {
		u := user{
			ID:   userID,
			Recv: make(chan slackEvent),
			Send: make(chan connReplyMsg),
		}
		users[userID] = u
		go userReceiveMessages(ctx, u)
		go userSendMessages(ctx, u)
	}
	for {
		m := <-ctx.UserEvents
		switch v := m.(type) {
		case slackEvent:
			u := users[v.User]
			if u != (user{}) {
				go func() {
					u.Recv <- v
				}()
			}
		case connToUserMsg:
			u := users[v.UserID]
			if u != (user{}) {
				go func() {
					u.Send <- connReplyMsg{Message: v.Message}
				}()
			}
		case connToAllUsersMsg:
			for _, u := range users {
				go func(u user) {
					u.Send <- connReplyMsg{Message: v.Message}
				}(u)
			}
		default:
			log.Warningf("User event unknown: %#v", v)
		}
	}
}

func slackHandler(ctx ctx, token string) {
	origin := "https://slack.com/api/rtm.start?token=" + token
	go func() {
		ctx.Restarts <- restart{Reason: "initial start"}
	}()
	for {
		msg := <-ctx.Restarts
		log.Infof("Restarting: %#v; no of goroutines: %d", msg, runtime.NumGoroutine())
		rtm := startRtm(origin)
		log.Debugf("RTM url: %s", rtm.URL)
		ws, err := connectWs(rtm.URL, origin)
		if err != nil {
			errorMsg := "Error trying to connect to WS"
			log.Errorf("%s: %v", errorMsg, err)
			go func() {
				ctx.Restarts <- restart{Reason: errorMsg}
			}()
			go func() {
				ctx.Connections <- connMessage{Type: "restart", Msg: errorMsg}
			}()
		} else {
			go eventsProducer(ws, ctx)
		}
		time.Sleep(10 * time.Second)
	}
}

func msgToJSONMsg(msg msg) string {
	json, _ := json.Marshal(msg)
	return (string(json) + "\n")
}

func jsonToMsg(jsonString string) (msg, error) {
	var msg msg
	decodeErr := json.NewDecoder(strings.NewReader(jsonString)).Decode(&msg)
	return msg, decodeErr
}

func runClient(ctx ctx, conn net.Conn) {
	defer func() {
		log.Infof("No of goroutines: %d", runtime.NumGoroutine())
		log.Info("Closing client")
	}()
	reader := bufio.NewReader(conn)
	registered := connRegister{}
	for {
		rawMsg, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			if registered != (connRegister{}) {
				go func() {
					ctx.Connections <- connUnregister{Name: registered.Name}
				}()
			}
			return
		}
		reply := msg{Type: "ok", Data: "ok"}
		message := strings.TrimSpace(rawMsg)
		decodedMsg, decodeErr := jsonToMsg(message)
		if decodeErr != nil {
			log.Warningf("Error while decoding message: %s; invalid message format: %s", decodeErr, message)
			reply = msg{Type: "error", Data: "invalid format"}
		} else {
			switch decodedMsg.Type {
			case "register":
				registered = connRegister{
					Name: decodedMsg.Data,
					Conn: conn,
				}
				go func() {
					ctx.Connections <- registered
				}()
			case "msg":
				go func() {
					if decodedMsg.User == "" {
						ctx.UserEvents <- connToAllUsersMsg{Message: decodedMsg.Data}
					} else {
						ctx.UserEvents <- connToUserMsg{
							UserID:  decodedMsg.User,
							Message: decodedMsg.Data,
						}
					}
				}()
			default:
				reply = msg{Type: "error", Data: "message unknown"}
			}
		}
		conn.Write([]byte(msgToJSONMsg(reply)))
		log.Debugf("message %#v", decodedMsg)
	}
}

func tcpHandler(host string, port int, ctx ctx) {
	listener, err := net.Listen("tcp", host+":"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("Error starting TCP server on host %s, port %d", host, port)
	}
	log.Infof("Started TCP listener on %s:%d", host, port)
	defer listener.Close()
	for {
		conn, _ := listener.Accept()
		go runClient(ctx, conn)
	}
}

func connectionsRegistrar(ctx ctx) {
	connections := make(map[string]net.Conn)
	for {
		connCmd := <-ctx.Connections
		switch v := connCmd.(type) {
		case connRegister:
			connections[v.Name] = v.Conn
		case connUnregister:
			delete(connections, v.Name)
		case connMessage:
			for key, conn := range connections {
				log.Infof("Sent message to connection: %s", key)
				txt := msgToJSONMsg(msg{
					Type: v.Type,
					Data: v.Msg,
					User: v.UserID,
				})
				go func(conn net.Conn) {
					conn.Write([]byte(txt))
				}(conn)
			}
		default:
			log.Infof("Unknown connection message type: %v", v)
		}
	}
}

func main() {
	config := readConfig()
	token := config.Token
	users := config.Users
	tcpport := config.Port
	tcphost := config.Host
	var wg sync.WaitGroup
	var format = logging.MustStringFormatter(
		`%{color}%{time:2006-01-02T15:04:05.000} %{shortfunc} >>> %{level} %{id:03x} %{message}%{color:reset}`,
	)
	loggingBackend := logging.NewLogBackend(os.Stderr, "", 0)
	backend2Formatter := logging.NewBackendFormatter(loggingBackend, format)
	loggingBackendLeveled := logging.AddModuleLevel(loggingBackend)
	loggingBackendLeveled.SetLevel(logging.DEBUG, "")
	logging.SetBackend(backend2Formatter)
	ctx := ctx{
		Restarts:    make(chan restart),
		SlackEvents: make(chan string),
		UserEvents:  make(chan interface{}),
		Connections: make(chan interface{}),
	}
	wg.Add(5)
	go func() {
		defer wg.Done()
		usersRegistrar(ctx, users)
	}()
	go func() {
		defer wg.Done()
		slackHandler(ctx, token)
	}()
	go func() {
		defer wg.Done()
		dispatchSlackEvents(ctx)
	}()
	go func() {
		defer wg.Done()
		tcpHandler(tcphost, tcpport, ctx)
	}()
	go func() {
		defer wg.Done()
		connectionsRegistrar(ctx)
	}()
	wg.Wait()
}
