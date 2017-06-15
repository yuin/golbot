package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/detached/gorocket/api"
	"github.com/detached/gorocket/realtime"
	"github.com/yuin/gopher-lua"
	"layeh.com/gopher-luar"
)

const rocketChatClientTypeName = "rocketChatClient"
const rocketDefaultConfigLua = `  myname = "golbot"
  local bot = golbot.newbot("Rocket", {
	url = "http://localhost:8080",
	name = myname,
    email  = "golbot@example.com",
    password = "password",
	channels = "general",
    log = {"seelog", type="adaptive", mininterval="200000000", maxinterval="1000000000", critmsgcount="5",
      {"formats",
        {"format", id="main", format="%Date(2006-01-02 15:04:05) [%Level] %Msg"},
      },
      {"outputs", formatid="main",
        {"filter", levels="trace,debug,info,warn,error,critical",
          {"console"}
        },
      }
    }
  })

  bot:on("message", function(e)
  end)
`

type rocketMessage struct {
	Id        string
	ChannelId string
	Channel   string
	Text      string
	Timestamp string
	User      api.User
}

type rocketRestClient struct {
	url       string
	authToken string
	authId    string
}

func newRocketRestClient(url string) *rocketRestClient {
	return &rocketRestClient{
		url:       url,
		authToken: "",
		authId:    "",
	}
}

func (c *rocketRestClient) Call(path string, p httpRequestParam) (map[string]interface{}, error) {
	p.Url = c.url + "/api/v1" + path
	if len(c.authToken) > 0 {
		if p.Headers == nil {
			p.Headers = []string{}
		}
		p.Headers = append(p.Headers, []string{"X-Auth-Token", c.authToken, "X-User-Id", c.authId}...)
	}
	res, err := httpRequest(p)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	bs, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return mustDecodeJson(bs), nil
}

func (c *rocketRestClient) Login(email, password string) error {
	res, err := c.Call("/login", httpRequestParam{Method: "POST", Data: nil,
		Params: []string{"user", email, "password", password}, Headers: nil})
	if err != nil {
		return err
	}
	if res["status"].(string) == "success" {
		c.authId = propertyPath(res, "data.userId").(string)
		c.authToken = propertyPath(res, "data.authToken").(string)
		return nil
	}
	return errors.New(fmt.Sprintf("Failed to Login as %s : %s", email, res["Status"].(string)))
}

type rocketChatClient struct {
	realtimeClient *realtime.Client
	restClient     *rocketRestClient
	commonOption   *CommonClientOption
	logger         *log.Logger
	callbacks      map[string][]*lua.LFunction
	name           string
	c2id           map[string]string
	id2c           map[string]string
	channels       []string
}

func (client *rocketChatClient) applyCallback(L *lua.LState, msg interface{}) {
	mutex.Lock()
	defer mutex.Unlock()
	typ := "message"
	switch msg.(type) {
	case api.Message:
		typ = "message"
	}
	v, ok := client.callbacks[typ]
	if !ok {
		return
	}
	for _, callback := range v {
		pushN(L, callback, luar.New(L, msg))
		if err := L.PCall(1, 0, nil); err != nil {
			client.logger.Printf("[ERROR] %s", err.Error())
		}
	}
}

func (client *rocketChatClient) Logger() *log.Logger {
	return client.logger
}

func (client *rocketChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *rocketChatClient) Say(target, message string) {
	client.realtimeClient.SendMessage(&api.Channel{Id: client.c2id[target]}, message)
}

func (client *rocketChatClient) On(L *lua.LState, typ string, callback *lua.LFunction) {
	mutex.Lock()
	defer mutex.Unlock()
	v, ok := client.callbacks[typ]
	if !ok {
		v = []*lua.LFunction{}
		client.callbacks[typ] = v
	}
	if typ == "message" {
		lastMsg := ""
		client.callbacks[typ] = append(v, L.NewFunction(func(L *lua.LState) int {
			e := L.CheckUserData(1).Value.(api.Message)
			if lastMsg == e.Id {
				return 0
			}
			lastMsg = e.Id
			rMsg := rocketMessage{e.Id, e.ChannelId, client.id2c[e.ChannelId], e.Text, e.Timestamp, e.User}
			lrMsg := luar.New(L, rMsg)
			pushN(L, callback, lrMsg)
			if err := L.PCall(1, 0, nil); err != nil {
				client.logger.Printf("[ERROR] %s", err.Error())
			}
			return 0
		}))
	} else {
		client.callbacks[typ] = append(v, callback)
	}
}

func (client *rocketChatClient) Respond(L *lua.LState, pattern *regexp.Regexp, fn *lua.LFunction) {
	client.On(L, "message", L.NewFunction(func(L *lua.LState) int {
		e := L.CheckUserData(1).Value.(rocketMessage)
		matches := pattern.FindAllStringSubmatch(e.Text, -1)
		mentionMe, _ := regexp.MatchString("@"+client.name+"\\s+", e.Text)
		if len(matches) > 0 && mentionMe {
			to := client.id2c[e.ChannelId]
			user := e.User.UserName
			if user == client.name {
				return 0
			}
			pushN(L, fn, luar.New(L, matches[0]), luar.New(L, NewMessageEvent(user, to, e.Text, e)))
			if err := L.PCall(2, 0, nil); err != nil {
				client.logger.Printf("[ERROR] %s", err.Error())
			}
		}
		return 0
	}))
}

func (client *rocketChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	realtimeClient := client.realtimeClient
	aggregator := make(chan api.Message)
	for _, channel := range client.channels {
		client.commonOption.Logger.Printf("[INFO] join to %s", channel)
		cid := client.c2id[channel]
		mc, err := realtimeClient.SubscribeToMessageStream(&api.Channel{Id: cid})
		if err != nil {
			client.commonOption.Logger.Printf("[ERROR] %s", err.Error())
			os.Exit(1)
		}
		go func(c chan api.Message) {
			for {
				select {
				case msg := <-c:
					aggregator <- msg
				}
			}
		}(mc)
	}

	for {
		select {
		case msg := <-aggregator:
			client.applyCallback(L, msg)
		case msg := <-luaMainChan:
			func() {
				mutex.Lock()
				defer mutex.Unlock()
				pushN(L, fn, msg)
				L.PCall(1, 0, nil)
			}()
		}
	}
}

func registerRocketChatClientType(L *lua.LState) {
	registerChatClientType(L, rocketChatClientTypeName)
	proxyLuar(L, realtime.Client{}, nil)
	proxyLuar(L, api.Channel{}, nil)
	proxyLuar(L, api.Info{}, nil)
	proxyLuar(L, api.Message{}, nil)
	proxyLuar(L, rocketMessage{}, nil)
	proxyLuar(L, api.User{}, nil)
	proxyLuar(L, api.UserCredentials{}, nil)
}

func newRocketChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	name, nok := getStringField(L, opt, "name")
	email, eok := getStringField(L, opt, "email")
	password, pok := getStringField(L, opt, "password")
	channels, cok := getStringField(L, opt, "channels")
	surl, uok := getStringField(L, opt, "url")
	if !nok || !pok || !eok || !uok || !cok {
		L.RaiseError("'url', 'name', 'email', 'password' are required")
	}
	u, err := url.Parse(surl)

	abortIfError := func(err error) {
		if err != nil {
			co.Logger.Printf("[ERROR] %s", err.Error())
			os.Exit(1)
		}
	}

	abortIfError(err)

	if co.Logger == nil {
		co.Logger = log.New(os.Stdout, "", log.Lshortfile|log.LstdFlags)
	}

	co.Logger.Printf("[INFO] login as %s(REST)", name)
	restClient := newRocketRestClient(surl)
	err = restClient.Login(email, password)
	abortIfError(err)

	co.Logger.Printf("[INFO] connect to %s", u.Host)
	realtimeClient, err := realtime.NewClient(u.Hostname(), u.Port(), false)
	abortIfError(err)

	co.Logger.Printf("[INFO] login as %s(Realtime)", name)
	err = realtimeClient.Login(&api.UserCredentials{Email: email, Name: name, Password: password})
	abortIfError(err)

	c2id := map[string]string{}
	co.Logger.Printf("[INFO] get available channel information")
	allChannels, err := restClient.Call("/channels.list", httpRequestParam{Method: "GET"})
	abortIfError(err)

	for _, channel := range asArray(allChannels["channels"]) {
		m := asObject(channel)
		co.Logger.Printf("[INFO] %s(id:%s)", m["name"].(string), m["_id"].(string))
		c2id[m["name"].(string)] = m["_id"].(string)
	}

	co.Logger.Printf("[INFO] get available group information")
	allGroups, err := restClient.Call("/groups.list", httpRequestParam{Method: "GET"})
	abortIfError(err)

	for _, group := range asArray(allGroups["groups"]) {
		m := asObject(group)
		co.Logger.Printf("[INFO] %s(id:%s)", m["name"].(string), m["_id"].(string))
		c2id[m["name"].(string)] = m["_id"].(string)
	}
	id2c := map[string]string{}
	for k, v := range c2id {
		id2c[v] = k
	}

	chatClient := &rocketChatClient{
		realtimeClient: realtimeClient,
		restClient:     restClient,
		commonOption:   co,
		logger:         co.Logger,
		callbacks:      make(map[string][]*lua.LFunction),
		name:           name,
		c2id:           c2id,
		id2c:           id2c,
		channels:       strings.Split(channels, ","),
	}
	L.Push(newChatClient(L, rocketChatClientTypeName, chatClient, luar.New(L, chatClient.realtimeClient).(*lua.LUserData)))
}
