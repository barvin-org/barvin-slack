package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/op/go-logging"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Restart struct {
	Reason string
}

type Ctx struct {
	Restarts    chan Restart
	Events      chan string
	Connections map[string]net.Conn
}

type Client struct {
	conn net.Conn
	name string
}

type Msg struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

var log = logging.MustGetLogger("main-logger")

type RtmResponse struct {
	Url string `json:"url"`
}

func startRtm(origin string) RtmResponse {
	resp, err := http.Get(origin)
	if err != nil {
		return RtmResponse{}
	}
	defer resp.Body.Close()
	var data RtmResponse
	json.NewDecoder(resp.Body).Decode(&data)
	return data
}

func connectWs(url string, origin string) (*websocket.Conn, error) {
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	return c, err
}

func eventsProducer(ws *websocket.Conn, ctx Ctx) {
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
			ctx.Restarts <- Restart{Reason: "slack read message error"}
			break
		}
		ctx.Events <- string(msg[:])
	}
}

func dispatchEvents(ctx Ctx) {
	for {
		event := <-ctx.Events
		log.Infof("Dispatching event: %s", event)
		for _, conn := range ctx.Connections {
			_, err := conn.Write([]byte(event))
			log.Infof("Write data to connection response: %s", err)
		}
	}
}

func slackHandler(ctx Ctx, token string) {
	origin := "https://slack.com/api/rtm.start?token=" + token
	go func() {
		ctx.Restarts <- Restart{Reason: "initial start"}
	}()
	for {
		msg := <-ctx.Restarts
		log.Infof("Restarting: %#v", msg)
		log.Infof("No of goroutines: %d", runtime.NumGoroutine())
		rtm := startRtm(origin)
		log.Debugf("RTM url: %s", rtm.Url)
		ws, err := connectWs(rtm.Url, origin)
		if err != nil {
			log.Errorf("Error trying to connect to WS: %v", err)
			go func() {
				ctx.Restarts <- Restart{Reason: "error connecting to WS"}
			}()
		} else {
			go eventsProducer(ws, ctx)
		}
		time.Sleep(10 * time.Second)
	}
}

func msgToJsonMsg(msg Msg) string {
	json, _ := json.Marshal(msg)
	return (string(json) + "\n")
}

func jsonToMsg(jsonString string) (Msg, error) {
	var msg Msg
	decodeErr := json.NewDecoder(strings.NewReader(jsonString)).Decode(&msg)
	return msg, decodeErr
}

func runClient(ctx Ctx, conn net.Conn) {
	defer func() {
		log.Infof("No of goroutines: %d", runtime.NumGoroutine())
		log.Info("Closing client")
	}()
	reader := bufio.NewReader(conn)
	for {
		rawMsg, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return
		}
		message := strings.TrimSpace(rawMsg)
		msg, decodeErr := jsonToMsg(message)
		if decodeErr != nil {
			log.Warningf("Error while decoding message: %s", decodeErr)
			log.Warningf("Invalid message format: %s", message)
			reply := Msg{Type: "error", Data: "invalid format"}
			conn.Write([]byte(msgToJsonMsg(reply)))
		} else {
			reply := Msg{Type: "ok", Data: "msg processed ok"}
			conn.Write([]byte(msgToJsonMsg(reply)))
		}
		log.Infof("message %#v", msg)
	}
}

func tcpHandler(host string, port int, ctx Ctx) {
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

func main() {
	userid := flag.String("userid", "", "The privileged slack userid.")
	token := flag.String("token", "", "Slack token to connect.")
	tcpport := flag.Int("port", 9191, "TCP listening port.")
	tcphost := flag.String("host", "localhost", "TCP listening host.")
	flag.Parse()
	if *token == "" {
		fmt.Println("Missing token.")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *userid == "" {
		fmt.Println("Missing userid.")
		flag.PrintDefaults()
		os.Exit(2)
	}
	var wg sync.WaitGroup
	var format = logging.MustStringFormatter(
		`%{color}%{time:2006-01-02T15:04:05.000} %{shortfunc} >>> %{level} %{id:03x} %{message}%{color:reset}`,
	)
	loggingBackend := logging.NewLogBackend(os.Stderr, "", 0)
	backend2Formatter := logging.NewBackendFormatter(loggingBackend, format)
	loggingBackendLeveled := logging.AddModuleLevel(loggingBackend)
	loggingBackendLeveled.SetLevel(logging.DEBUG, "")
	logging.SetBackend(backend2Formatter)
	ctx := Ctx{
		Restarts: make(chan Restart),
		Events:   make(chan string),
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		dispatchEvents(ctx)
	}()
	go func() {
		defer wg.Done()
		slackHandler(ctx, *token)
	}()
	go func() {
		defer wg.Done()
		tcpHandler(*tcphost, *tcpport, ctx)
	}()
	wg.Wait()
}
