package main

import (
	"log"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/thoj/go-ircevent"
	"github.com/yuin/gopher-lua"
	"layeh.com/gopher-luar"
)

const ircChatClientTypeName = "ircChatClient"
const ircDefaultConfigLua = `  mynick = "golbot"
  myname = "golbot"

  local bot = golbot.newbot("IRC", {
    nickname = mynick,
    username = myname,
    conn = "localhost:6667,#test",
    useTLS = false,
    password = "password",
    http = "0.0.0.0:6669",
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

  bot:on("PRIVMSG", function(e)
    local ch = e.arguments[1]
    local nick = e.nick
    local user = e.user
    local source = e.source
    local msg = e:message()
    if nick == mynick then
      return
    end

    msglog:printf("%s\t%s\t%s", ch, source, msg)
    bot.raw:privmsg(ch, msg)
	goworker({channel=ch, message=msg, nick=nick})
  end)

`

type ircChatClient struct {
	ircobj       *irc.Connection
	commonOption *CommonClientOption
	conn         string
	nick         string
}

func (client *ircChatClient) Logger() *log.Logger {
	return client.ircobj.Log
}

func (client *ircChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *ircChatClient) Say(target, message string) {
	client.ircobj.Privmsg(target, message)
}

func (client *ircChatClient) On(L *lua.LState, action string, fn *lua.LFunction) {
	client.ircobj.AddCallback(action, func(e *irc.Event) {
		mutex.Lock()
		defer mutex.Unlock()
		pushN(L, fn, luar.New(L, e))
		L.PCall(1, 0, nil)
	})
}

func (client *ircChatClient) Respond(L *lua.LState, pattern *regexp.Regexp, fn *lua.LFunction) {
	client.ircobj.AddCallback("PRIVMSG", func(e *irc.Event) {
		mutex.Lock()
		defer mutex.Unlock()
		matches := pattern.FindAllStringSubmatch(e.Message(), -1)
		mentionMe, _ := regexp.MatchString("[@:\\\\]"+client.nick+"\\s+", e.Message())
		if len(matches) != 0 && mentionMe {
			pushN(L, fn, luar.New(L, matches[0]), luar.New(L, NewMessageEvent(e.Nick, e.Arguments[0], e.Message(), e)))
			if err := L.PCall(2, 0, nil); err != nil {
				client.ircobj.Log.Printf("[ERROR] %s", err.Error())
			}
		}
	})
}

func (client *ircChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	irc := client.ircobj

	if len(irc.Server) == 0 {
		parts := strings.Split(client.conn, ",")
		if err := irc.Connect(parts[0]); err != nil {
			L.RaiseError(err.Error())
		}
		for i := 1; i < len(parts); i++ {
			irc.Join(parts[i])
		}
	}

	v := reflect.ValueOf(*irc)
	errChan := irc.ErrorChan()
	for !v.FieldByName("quit").Bool() {
		select {
		case err := <-errChan:
			irc.Log.Printf("Error, disconnected: %s\n", err)
			for !v.FieldByName("quit").Bool() {
				if err = irc.Reconnect(); err != nil {
					irc.Log.Printf("Error while reconnecting: %s\n", err)
					time.Sleep(60 * time.Second)
				} else {
					errChan = irc.ErrorChan()
					break
				}
			}
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

func registerIRCChatClientType(L *lua.LState) {
	registerChatClientType(L, ircChatClientTypeName)
	proxyLuar(L, irc.Connection{}, nil)
	proxyLuar(L, irc.Event{}, nil)
}

func newIRCChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	nickname, nok := getStringField(L, opt, "nickname")
	username, uok := getStringField(L, opt, "username")
	if !nok || !uok {
		L.RaiseError("'nickname' and 'username' are required")
	}

	ircobj := irc.IRC(nickname, username)
	chatClient := &ircChatClient{ircobj, co, "127.0.0.1:6667", nickname}

	if co.Logger != nil {
		ircobj.Log = co.Logger
	}
	ircobj.UseTLS = lua.LVAsBool(L.GetField(opt, "useTLS"))
	if s, ok := getStringField(L, opt, "password"); ok {
		ircobj.Password = s
	}
	if s, ok := getStringField(L, opt, "conn"); ok {
		chatClient.conn = s
	}
	L.Push(newChatClient(L, ircChatClientTypeName, chatClient, luar.New(L, ircobj).(*lua.LUserData)))
}
