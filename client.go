package signalr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/carterjones/helpers/trace"
	"github.com/gorilla/websocket"
)

const (
	serverInitialized = 1
)

type negotiateResponse struct {
	URL                     string `json:"Url"`
	ConnectionToken         string
	ConnectionID            string `json:"ConnectionId"`
	KeepAliveTimeout        float64
	DisconnectTimeout       float64
	ConnectionTimeout       float64
	TryWebSockets           bool
	ProtocolVersion         string
	TransportConnectTimeout float64
	LongPollDelay           float64
}

type startResponse struct {
	Response string
}

func (nr *negotiateResponse) connectionTokenEscaped() string {
	return url.QueryEscape(nr.ConnectionToken)
}

// PersistentConnectionMessage represents a message sent from the server to the
// websocket connection.
type PersistentConnectionMessage struct {
	// message id, present for all non-KeepAlive messages
	C string

	// an array containing actual data
	M []HubsClientMessage

	// indicates that the transport was initialized (a.k.a. init message)
	S int

	// groups token – an encrypted string representing group membership
	G string
}

// HubsClientMessage represents a message sent to the Hubs API from the client.
type HubsClientMessage struct {
	// invocation identifier – allows to match up responses with requests
	I int

	// the name of the hub
	H string

	// the name of the method
	M string

	// arguments (an array, can be empty if the method does not have any
	// parameters)
	A []interface{}

	// state – a dictionary containing additional custom data (optional)
	S *json.RawMessage `json:",omitempty"`
}

// MarshalJSON converts the current message into a JSON-formatted byte array. It
// will perform different types of conversion based on the Golang type of the
// "A" field. For instance, an array will be converted into a JSON object
// looking like [...], whereas a byte array would look like "...".
func (hcm *HubsClientMessage) MarshalJSON() (buf []byte, err error) {
	var args []byte
	for _, a := range hcm.A {
		switch a.(type) {
		case []byte:
			args = append(args, a.([]byte)...)
		case string:
			args = append(args, []byte(a.(string))...)
		default:
			err = errors.New("unsupported argument type")
			trace.Error(err)
			return
		}
	}

	return json.Marshal(&struct {
		I int
		H string
		M string
		A []byte
		S *json.RawMessage `json:"omitempty"`
	}{
		I: hcm.I,
		H: hcm.H,
		M: hcm.M,
		A: args,
		S: hcm.S,
	})
}

// HubsServerMessage represents a message sent to the Hubs API from the server.
type HubsServerMessage struct {
	// invocation Id (always present)
	I int

	// the value returned by the server method (present if the method is not
	// void)
	R *json.RawMessage `json:",omitempty"`

	// error message
	E *string `json:",omitempty"`

	// true if this is a hub error
	H *bool `json:",omitempty"`

	// an object containing additional error data (can only be present for
	// hub errors)
	D *json.RawMessage `json:",omitempty"`

	// stack trace (if detailed error reporting (i.e. the
	// HubConfiguration.EnableDetailedErrors property) is turned on on the
	// server)
	T *json.RawMessage `json:",omitempty"`

	// state – a dictionary containing additional custom data (optional)
	S *json.RawMessage `json:",omitempty"`
}

// Client represents a SignlR client. It manages connections so you don't have
// to!
type Client struct {
	host     string
	protocol string

	connectionData string

	conn *websocket.Conn

	messages chan PersistentConnectionMessage
}

func (c *Client) setConnectionData(cd string) {
	c.connectionData = url.QueryEscape(cd)
}

func (c *Client) negotiate() (nr negotiateResponse, err error) {
	uri := "https://" + c.host +
		"/signalr/negotiate?clientProtocol=" + c.protocol +
		"&connectionData=" + c.connectionData

	for i := 0; i < 5; i++ {
		var resp *http.Response
		resp, err = http.Get(uri)
		if err != nil {
			trace.Error(err)
			return
		}

		if resp.Status != "200 OK" {
			trace.DebugMessage("non-200 response while negotiating: " + resp.Status)
			time.Sleep(time.Minute)
			continue
		}

		defer func() {
			derr := resp.Body.Close()
			if derr != nil {
				trace.Error(derr)
			}
		}()

		var body []byte
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			trace.Error(err)
			return
		}

		err = json.Unmarshal(body, &nr)
		if err != nil {
			trace.Error(err)
			return
		}

		return
	}

	return
}

func (c *Client) connect(nr negotiateResponse) (conn *websocket.Conn, err error) {
	path := nr.URL +
		"/connect?transport=webSockets&clientProtocol=" + c.protocol +
		"&connectionToken=" + nr.connectionTokenEscaped() +
		"&connectionData=" + c.connectionData
	url := "wss://" + c.host + path

	conn, resp, err := websocket.DefaultDialer.Dial(url, http.Header{})
	if err != nil {
		trace.Error(err)

		if err == websocket.ErrBadHandshake {
			defer func() {
				derr := resp.Body.Close()
				if derr != nil {
					trace.Error(derr)
				}
			}()

			body, err2 := ioutil.ReadAll(resp.Body)
			if err != nil {
				trace.Error(err2)
				err = err2
				return
			}

			log.Println(string(body))
			log.Println(resp)
			log.Println(resp.Request)
			return
		}
	}

	return
}

func (c *Client) start(nr negotiateResponse, conn *websocket.Conn) (err error) {
	path := nr.URL +
		"/start?transport=webSockets&clientProtocol=" + c.protocol +
		"&connectionToken=" + nr.connectionTokenEscaped() +
		"&connectionData=" + c.connectionData
	url := "https://" + c.host + path

	resp, err := http.Get(url)
	if err != nil {
		trace.Error(err)
		return
	}

	defer func() {
		derr := resp.Body.Close()
		if derr != nil {
			trace.Error(derr)
		}
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		trace.Error(err)
		return
	}

	var sr startResponse
	err = json.Unmarshal(body, &sr)
	if err != nil {
		trace.Error(err)
		return
	}

	// Confirm the server response is what we expect.
	if sr.Response != "started" {
		err = errors.New("start response is not 'started': " + sr.Response)
		trace.Error(err)
		return
	}

	// Wait for the init message.
	t, p, err := conn.ReadMessage()
	if err != nil {
		trace.Error(err)
		return
	}

	// Verify the correct response type was received.
	if t != websocket.TextMessage {
		err = errors.New("unexpected websocket control type:" + strconv.Itoa(t))
		trace.Error(err)
		return
	}

	// Extract the server message.
	var pcm PersistentConnectionMessage
	err = json.Unmarshal(p, &pcm)
	if err != nil {
		trace.Error(err)
		return
	}

	if pcm.S != serverInitialized {
		err = errors.New("unexpected S value received from server: " + strconv.Itoa(pcm.S))
		trace.Error(err)
		return
	}

	// Since we got to this point, the connection is successful. So we set
	// the connection for the client.
	c.conn = conn
	return
}

// func (c *Client) reconnect() {
// TBD if this is needed. Note from
// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
// Once the channel is set up there are no further HTTP requests until
// the client is stopped (the abort request) or the connection was lost
// and the client tries to re-establish the connection (the reconnect
// request).
// }

func (c *Client) init(host, protocol, connectionData string) (err error) {
	c.host = host
	c.protocol = protocol
	c.setConnectionData(connectionData)
	c.messages = make(chan PersistentConnectionMessage)

	nr, err := c.negotiate()
	if err != nil {
		trace.Error(err)
		return
	}

	conn, err := c.connect(nr)
	if err != nil {
		trace.Error(err)
		return
	}

	err = c.start(nr, conn)
	return
}

func (c *Client) readMessages() {
	for {
		trace.DebugMessage("[signalR.readMessages] Waiting for message...")

		_, p, err := c.conn.ReadMessage()
		if err != nil {
			trace.Error(err)
			return
		}

		trace.DebugMessage("[signalR.readMessages] Message received: " + string(p))

		// Ignore KeepAlive messages.
		if string(p) == "{}" {
			continue
		}

		var pcm PersistentConnectionMessage
		err = json.Unmarshal(p, &pcm)
		if err != nil {
			trace.Error(err)
			return
		}

		dbgMsg := fmt.Sprintf("%v", pcm)
		trace.DebugMessage("[signalR.readMessages] " + dbgMsg)

		c.messages <- pcm
	}
}

// Write sends a message to the connection.
func (c *Client) Write(m HubsClientMessage) (err error) {
	err = c.conn.WriteJSON(m)
	if err != nil {
		trace.Error(err)
		return
	}
	return
}

// Messages returns the channel that receives persistent connection messages.
func (c *Client) Messages() <-chan PersistentConnectionMessage {
	return c.messages
}

// New creates and initializes a SignalR client. It connects to the host and
// performs the websocket initialization routines that are part of the SignalR
// specification.
func New(host, protocol, connectionData string) (c Client) {
	err := c.init(host, protocol, connectionData)
	if err != nil {
		trace.Error(err)
		return
	}

	go c.readMessages()

	return
}