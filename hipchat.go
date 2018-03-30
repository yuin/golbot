package main

import (
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/daneharrigan/hipchat"
	"github.com/yuin/gopher-lua"
	"layeh.com/gopher-luar"
)

const hipchatChatClientTypeName = "hipchatChatClient"
const hipchatDefaultConfigLua = `  myname = "golbot"
  local bot = golbot.newbot("Hipchat", {
	user = "111111_111111",
	password = "password",
	host = "chat.hipchat.com",
	conf = "conf.hipchat.com",
	auth_type = "plain",
	room_jids = {"111111_xxxxx@conf.hipchat.com", "1111111_yyyyyy@conf.hipchat.com"},
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
    local i = (e.from or ""):find("/")
    if i == nil then
      return
    end
    local to = e.from:sub(1, i-1)
    local user = e.from:sub(i+1, -1)
    local msg = e.body

    if user == myname then
      return
    end

    msglog:printf("%s\t%s\t%s", to, user, msg)
    bot:say(to, msg)
    goworker({channel=to, message=msg, user=user})
  end)
`

type hipchatChatClient struct {
	hipchatobj   *hipchat.Client
	commonOption *CommonClientOption
	logger       *log.Logger
	callbacks    map[string][]*lua.LFunction
	roomsJids    []string
	name         string
	mentionName  string
}

func (client *hipchatChatClient) applyCallback(L *lua.LState, msg interface{}) {
	mutex.Lock()
	defer mutex.Unlock()
	typ := "message"
	switch msg.(type) {
	case *hipchat.Message:
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

func (client *hipchatChatClient) Logger() *log.Logger {
	return client.logger
}

func (client *hipchatChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *hipchatChatClient) Say(target, message string) {
	client.hipchatobj.Say(target, client.name, message)
}

func (client *hipchatChatClient) On(L *lua.LState, typ string, callback *lua.LFunction) {
	mutex.Lock()
	defer mutex.Unlock()
	v, ok := client.callbacks[typ]
	if !ok {
		v = []*lua.LFunction{}
		client.callbacks[typ] = v
	}
	client.callbacks[typ] = append(v, callback)
}

func (client *hipchatChatClient) Respond(L *lua.LState, pattern *regexp.Regexp, fn *lua.LFunction) {
	client.On(L, "message", L.NewFunction(func(L *lua.LState) int {
		e := L.CheckUserData(1).Value.(*hipchat.Message)
		matches := pattern.FindAllStringSubmatch(e.Body, -1)
		mentionMe, _ := regexp.MatchString("@"+client.mentionName+"\\s+", e.Body)
		if len(matches) > 0 && mentionMe {
			to := strings.Split(e.From, "/")[0]
			user := strings.Split(e.From, "/")[1]
			if user == client.name {
				return 0
			}
			pushN(L, fn, luar.New(L, matches[0]), luar.New(L, NewMessageEvent(user, to, e.Body, e)))
			if err := L.PCall(2, 0, nil); err != nil {
				client.logger.Printf("[ERROR] %s", err.Error())
			}
		}
		return 0
	}))
}

func (client *hipchatChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	hipchatobj := client.hipchatobj
	hipchatobj.RequestUsers()

	for {
		select {
		case users := <-hipchatobj.Users():
			for _, user := range users {
				if user.Id == hipchatobj.Id {
					client.name = user.Name
					client.mentionName = user.MentionName
				}
			}
			client.Logger().Printf("[INFO] name=%s, mention name=%s", client.name, client.mentionName)

			for _, jid := range client.roomsJids {
				hipchatobj.Join(jid, client.name)
			}
			hipchatobj.Status("chat")
		case msg := <-hipchatobj.Messages():
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

func registerHipchatChatClientType(L *lua.LState) {
	registerChatClientType(L, hipchatChatClientTypeName)
}

func newHipchatChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	user, uok := getStringField(L, opt, "user")
	password, pok := getStringField(L, opt, "password")
	if !uok || !pok {
		L.RaiseError("'user' and 'password' are required")
	}
	host, _ := getStringField(L, opt, "host")
	if len(host) == 0 {
		host = "chat.hipchat.com"
	}
	conf, _ := getStringField(L, opt, "conf")
	if len(conf) == 0 {
		conf = "conf.hipchat.com"
	}
	resource, _ := getStringField(L, opt, "resource")
	if len(resource) == 0 {
		resource = "bot"
	}
	authType, _ := getStringField(L, opt, "auth_type")
	if len(resource) == 0 {
		authType = "plain"
	}
	roomJids := L.GetField(opt, "room_jids")

	if co.Logger == nil {
		co.Logger = log.New(os.Stdout, "", log.Lshortfile|log.LstdFlags)
	}
	hipchatobj, err := hipchat.NewClient(user, password, resource, authType)
	if err != nil {
		co.Logger.Printf("[ERROR] %s", err.Error())
		os.Exit(1)
	}
	co.Logger.Printf("[INFO] connected to %s", host)
	chatClient := &hipchatChatClient{hipchatobj, co, co.Logger, make(map[string][]*lua.LFunction), []string{}, "", ""}
	if tbl, ok := roomJids.(*lua.LTable); ok {
		tbl.ForEach(func(key, value lua.LValue) {
			chatClient.roomsJids = append(chatClient.roomsJids, value.String())
		})
	}

	L.Push(newChatClient(L, hipchatChatClientTypeName, chatClient, luar.New(L, chatClient.hipchatobj).(*lua.LUserData)))
}
