package main

import (
	"encoding/json"
	"expvar"
	"flag"
	"log"
	"net"
	"net/http"
	nurl "net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/donovanhide/eventsource"
	"github.com/fiorix/freegeoip"
	"github.com/monnand/goredis"
)

const (
	// ssePath is the PATH for the eventsource handler is going to be mounted on
	ssePath = "/push/"
)

var (
	numRegisterUsers   = expvar.NewInt("num_register_users")
	numUnregisterUsers = expvar.NewInt("num_unregister_users")
	numMessages        = expvar.NewInt("num_messages")
)

// Message is the bit of information that is transfered via eventsource
type Message struct {
	Idx           string
	Channel, HTML string
}

// Id is required to implement the eventsource.Event interface
func (c *Message) Id() string { return c.Idx }

// Event is required to implement the eventsource.Event interface
func (c *Message) Event() string { return c.Channel }

// Data is required to implement the eventsource.Event interface
func (c *Message) Data() string {
	return c.HTML
}

// Connection is use to relate a user token to a channel
type Connection struct {
	token   string
	channel string
}

type Location struct {
	Latitude  float64
	Longitude float64
	timezone  string
}

// Hub maintains the states
type Hub struct {
	Data       map[string][]string // Key is the channel, value is a slice of token
	Users      map[string]string   // Key is the token, value is a channel
	register   chan Connection
	unregister chan string
	messages   chan goredis.Message
	srv        *eventsource.Server
	client     goredis.Client
	ipdb       *freegeoip.DB
}

func (h *Hub) userExists(token string) bool {
	_, ok := h.Users[token]
	return ok
}

func (h *Hub) run() {
	log.Println("[Info] Start the Hub")
	var payload [4]string
	psub := make(chan string, 0)
	go h.client.Subscribe(nil, nil, psub, nil, h.messages)

	// Listening to all channel updates
	psub <- "channel_update:*"

	for {
		select {
		case conn := <-h.register:
			log.Println("[Info] register user: ", conn.token)
			h.Users[conn.token] = conn.channel
			h.Data[conn.channel] = append(h.Data[conn.channel], conn.token)
			numRegisterUsers.Add(1)

		case token := <-h.unregister:
			log.Println("[Info] Unregister user: ", token)
			ch, ok := h.Users[token]
			if ok {
				delete(h.Users, token)
				delete(h.Data, ch)
			}
			numUnregisterUsers.Add(1)

		case msg := <-h.messages:
			err := json.Unmarshal(msg.Message, &payload)
			if err != nil {
				log.Println("[Error] An error occured while Unmarshalling the msg: ", msg)
			}
			message := &Message{
				Idx:     payload[2],
				Channel: payload[0],
				HTML:    payload[1],
			}

			val, ok := h.Data[msg.Channel]
			if ok && len(val) >= 1 {
				//log.Println("[Debug] Publishing message", message)
				h.srv.Publish(val, message)
			}

			if h.ipdb != nil {
				// lookup the host
				ips, err := net.LookupIP(payload[3])
				if err == nil && len(ips) > 0 {
					var result map[string]interface{}
					err = h.ipdb.Lookup(ips[0], &result)
					if err != nil {
						log.Println(err)
					} else {
						location := reflect.ValueOf(result["location"])
						lat := location.Interface().(map[string]interface{})["latitude"]
						lon := location.Interface().(map[string]interface{})["longitude"]

						val, _ := json.Marshal([]float64{reflect.ValueOf(lat).Interface().(float64),
							reflect.ValueOf(lon).Interface().(float64)})
						message2 := &Message{
							Idx:     payload[2],
							Channel: "loc",
							HTML:    string(val),
						}

						log.Println("[Debug] Sending ip to glob ", message2)
						h.srv.Publish([]string{"glob"}, message2)
					}
				} else {
					log.Println("[Error] Cannot resolve host :'", payload[3], "'. ", err)
				}
			}

			numMessages.Add(1)
		}
	}
}

// EventSourceHandler implements the Handler interface
func (h *Hub) EventSourceHandler(w http.ResponseWriter, req *http.Request) {
	token := req.URL.Path[len(ssePath):]

	// The reserved name "glob" is accepted as a global channel that just sends stats
	if token == "glob" {
		log.Println("[Info] Connecting to the global channel")
		h.register <- Connection{"_", "glob"} // TODO: Need to come up with random, unique user ids
		defer func(u string) {
			h.unregister <- u
		}("_")
		h.srv.Handler(token)(w, req)

	} else {
		if h.userExists(token) {
			log.Println("[Info] Forbiden, user already connected")
			http.Error(w, "Forbiden", http.StatusForbidden)
		} else {
			log.Println("[Info] Exchange token against the channel list", token)
			val, err := h.client.Getset(token, []byte{})
			if err != nil {
				log.Println("[Error] occured while exchanging the your security token.", token, ":", err)
				http.Error(w, "Error occured while exchanging the your security token", http.StatusUnauthorized)

			} else if chanName := string(val); chanName != "" {
				log.Println("[Info] Connecting", token, "to the channel", chanName)
				h.register <- Connection{token, chanName}
				defer func(u string) {
					h.unregister <- u
				}(token)
				h.srv.Handler(token)(w, req)
			}
			_, err = h.client.Del(token)
			if err != nil {
				log.Println("[Error] An error occured while trying to delete the token from redis", err)
			}
		}
	}
}

// NewHub returns a pointer to a initialized and running Hub
func NewHub() *Hub {
	// Handle the flags. TODO: add flags for redis
	var dbPath string
	flag.StringVar(&dbPath, "ipdb-path", os.Getenv("IPDB_PATH"),
		"The path to the ip->location DB")
	flag.Parse()

	// Initialze the hub
	log.Println("[Info] Starting the eventsource Hub")

	var db *freegeoip.DB
	var err error
	if dbPath != "" {
		log.Println("[Info] Using ip->location DB ", dbPath)
		db, err = freegeoip.Open(dbPath)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		log.Println("[Info] No ip->location DB provided")
	}

	redisURLString := os.Getenv("REDIS_SSEQUEUE_URL")
	if redisURLString == "" {
		// Use db 2 by default for  pub/sub
		redisURLString = "redis://localhost:6379/2"
	}
	log.Println("[Info] Redis configuration used for pub/sub", redisURLString)
	redisURL, err := nurl.Parse(redisURLString)
	if err != nil {
		log.Fatal("Could not read Redis string", err)
	}

	redisDb, err := strconv.Atoi(strings.TrimLeft(redisURL.Path, "/"))
	if err != nil {
		log.Fatal("[Error] Could not read Redis path", err)
	}

	server := eventsource.NewServer()
	server.AllowCORS = true

	h := Hub{
		Data:       make(map[string][]string),
		Users:      make(map[string]string),
		register:   make(chan Connection, 0),
		unregister: make(chan string, 0),
		messages:   make(chan goredis.Message, 0),
		srv:        server,
		client:     goredis.Client{Addr: redisURL.Host, Db: redisDb},
		ipdb:       db,
	}

	go h.run()
	return &h
}

func main() {
	sseString := os.Getenv("SSE_HOST")

	if sseString == "" {
		log.Fatal("SSE_HOST is not set, example: SSE_HOST=localhost:3000")
	}
	h := NewHub()

	// eventsource endpoints
	log.Println("[Info] botbot-eventsource is listening on " + sseString)
	http.HandleFunc(ssePath, h.EventSourceHandler)

	log.Fatalln(http.ListenAndServe(sseString, nil))
}
