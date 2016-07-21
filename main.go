package main

import (
	"flag"
	"fmt"
	"github.com/cihub/seelog"
	"github.com/cjoudrey/gluahttp"
	"github.com/kohkimakimoto/gluafs"
	luajson "github.com/layeh/gopher-json"
	"github.com/layeh/gopher-luar"
	"github.com/otm/gluash"
	"github.com/thoj/go-ircevent"
	"github.com/yuin/charsetutil"
	"github.com/yuin/gluare"
	"github.com/yuin/gopher-lua"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

const defaultConfigLua string = `local golbot = require("golbot")
local json = require("json")
local charset = require("charset")
-- local re = require("re")
-- local httpclient = require("http")
-- local sh = require("sh")
-- local fs = require("fs")

function main()
  mynick = "golbot"
  myname = "golbot"
  channels = {"#test"}
  msglog = golbot.newlogger({"seelog", type="adaptive", mininterval="200000000", maxinterval="1000000000", critmsgcount="5",
      {"formats",
        {"format", id="main", format="%Date(2006-01-02 15:04:05) %Msg"},
      },
      {"outputs", formatid="main",
        {"filter", levels="trace,debug,info,warn,error,critical",
          {"console"}
        },
      }
  })

  local bot = golbot.newbot("IRC", mynick, myname, {
    useTLS = false,
    password = "password",
    worker = 3,
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

  bot:connect("localhost:6667")
  for i, ch in ipairs(channels) do
    bot:join(ch)
    bot:privmsg(ch, "hello all!")
  end

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
    bot:privmsg(ch, msg)
    notifywoker({channel=ch, message=msg, nick=nick})
  end)

  bot:serve(function(msg)
    if msg.type == "PRIVMSG" then
      bot:privmsg(msg.channel, msg.message)
      respond(msg, true)
    end
  end)
end

function worker(msg)
  notifymain({type="PRIVMSG", channel=msg.channel, message="accepted"})
end

function http(method, url, reader)
  if method == "POST" and url.path == "/privmsg" then
    local msg = json.decode(reader())
    local ok, success = requestmain({type="PRIVMSG", channel=msg.channel, message=msg.message, result=result})
    if ok and success then
      return 200,
             {
               {"Content-Type", "application/json; charset=utf-8"}
             },
             json.encode({result="ok"})
    else
      return 406,
             {
               {"Content-Type", "application/json; charset=utf-8"}
             },
             json.encode({result="error"})
    end
  end
  return 400,
         {
           {"Content-Type", "text/plain; charset=utf-8"}
         },
         "NOT FOUND"
end
`

var luaMainChan chan lua.LValue
var luaWorkerChan chan lua.LValue

var mainL *lua.LState
var mutex sync.Mutex

var charsetMod map[string]lua.LGFunction = map[string]lua.LGFunction{
	"decode": func(L *lua.LState) int {
		bytes, err := charsetutil.DecodeString(L.CheckString(1), L.CheckString(2))
		if err != nil {
			L.RaiseError(err.Error())
		}
		L.Push(lua.LString(string(bytes)))
		return 1
	},
	"encode": func(L *lua.LState) int {
		s, err := charsetutil.EncodeString(L.CheckString(1), L.CheckString(2))
		if err != nil {
			L.RaiseError(err.Error())
		}
		L.Push(lua.LString(s))
		return 1
	},
}

func pushN(L *lua.LState, values ...lua.LValue) {
	for _, v := range values {
		L.Push(v)
	}
}

func getStringField(L *lua.LState, t lua.LValue, key string) (string, bool) {
	lv := L.GetField(t, key)
	if s, ok := lv.(lua.LString); ok {
		return string(s), true
	}
	return "", false
}

func getNumberField(L *lua.LState, t lua.LValue, key string) (float64, bool) {
	lv := L.GetField(t, key)
	if n, ok := lv.(lua.LNumber); ok {
		return float64(n), true
	}
	return 0, false
}

func toCamel(s string) string {
	return strings.Replace(strings.Title(strings.Replace(s, "_", " ", -1)), " ", "", -1)
}

type luaLogger struct {
	L  *lua.LState
	fn *lua.LFunction
}

func (ll *luaLogger) Write(p []byte) (int, error) {
	pushN(ll.L, ll.fn, lua.LString(string(p)))
	err := ll.L.PCall(1, 0, nil)
	return len(p), err
}

type seelogLogger struct {
	l seelog.LoggerInterface
}

func (sl *seelogLogger) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '[' {
		parts := strings.Split(string(p), " ")
		switch parts[0] {
		case "[TRACE]", "[trace]":
			sl.l.Trace(strings.Join(parts[1:], " "))
		case "[DEBUG]", "[debug]":
			sl.l.Debug(strings.Join(parts[1:], " "))
		case "[INFO]", "[info]":
			sl.l.Info(strings.Join(parts[1:], " "))
		case "[WARN]", "[warn]":
			sl.l.Warn(strings.Join(parts[1:], " "))
		case "[ERROR]", "[error]":
			sl.l.Error(strings.Join(parts[1:], " "))
		case "[CRITICAL]", "[critical]":
			sl.l.Critical(strings.Join(parts[1:], " "))
		default:
			sl.l.Info(strings.Join(parts, " "))
		}
	} else {
		sl.l.Info(string(p))
	}
	return 0, nil
}

func luaToXml(lvalue lua.LValue) string {
	buf := []string{}
	return strings.Join(_luaToXml(lvalue, buf), " ")
}

func _luaToXml(lvalue lua.LValue, buf []string) []string {
	switch v := lvalue.(type) {
	case *lua.LTable:
		tag := v.RawGetInt(1).String()
		buf = append(buf, fmt.Sprintf("<%s", tag))
		v.ForEach(func(key, value lua.LValue) {
			switch kv := key.(type) {
			case lua.LNumber:
			default:
				buf = append(buf, fmt.Sprintf(" %s=\"%s\"", kv.String(), value.String()))
			}
		})
		buf = append(buf, ">")
		v.ForEach(func(key, value lua.LValue) {
			if kv, ok := key.(lua.LNumber); ok {
				if kv == 1 {
					return
				}
				if s, ok := key.(lua.LString); ok {
					buf = append(buf, s.String())
				} else {
					buf = _luaToXml(value, buf)
				}
			}
		})
		buf = append(buf, fmt.Sprintf("</%s>", tag))
	}
	return buf
}

func proxyLuar(L *lua.LState, tp interface{}, methods func(*lua.LState, string) bool) {
	mt := luar.MT(L, tp)
	newIndexFn := mt.RawGetString("__index")
	indexFn := mt.RawGetString("__index")
	mt.RawSetString("__newindex", L.NewFunction(func(L *lua.LState) int {
		pushN(L, newIndexFn, L.Get(1), lua.LString(toCamel(L.CheckString(2))), L.Get(3))
		L.Call(3, 0)
		return 0
	}))

	mt.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		if !methods(L, key) {
			pushN(L, indexFn, L.Get(1), lua.LString(toCamel(key)))
			L.Call(2, 1)
		}
		return 1
	}))
}

func nullProxy(L *lua.LState, key string) bool { return false }

type httpHandler struct {
	ircobj *irc.Connection
	conf   string
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ircobj.Log.Printf("[INFO] HTTP %s %s %s %s ", r.RemoteAddr, r.Method, r.RequestURI, r.Proto)
	L := newLuaState(h.conf)
	defer L.Close()
	pushN(L, L.GetGlobal("http"), lua.LString(r.Method), luar.New(L, r.URL),
		L.NewFunction(func(L *lua.LState) int {
			b, err := ioutil.ReadAll(r.Body)
			if err != nil {
				pushN(L, lua.LNil, lua.LString(err.Error()))
				return 2
			}
			pushN(L, lua.LString(b))
			return 1
		}))
	err := L.PCall(3, 3, nil)
	if err != nil {
		h.ircobj.Log.Printf("[ERROR] %s", err.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	} else {
		L.CheckTable(-2).ForEach(func(k, v lua.LValue) {
			t := v.(*lua.LTable)
			w.Header().Add(t.RawGetInt(1).String(), t.RawGetInt(2).String())
		})
		w.WriteHeader(int(L.CheckNumber(-3)))
		fmt.Fprint(w, L.CheckString(-1))
	}
}

func newLuaState(conf string) *lua.LState {
	L := lua.NewState()
	nworker := 3
	httpaddr := ""
	mod := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"newbot": func(L *lua.LState) int {
			// typ := L.CheckString(1)
			ircobj := irc.IRC(L.CheckString(2), L.CheckString(3))
			opt := L.OptTable(4, L.NewTable())

			ircobj.UseTLS = lua.LVAsBool(L.GetField(opt, "useTLS"))
			if s, ok := getStringField(L, opt, "password"); ok {
				ircobj.Password = s
			}
			switch v := L.GetField(opt, "log").(type) {
			case *lua.LFunction:
				ircobj.Log = log.New(&luaLogger{L, v}, "", log.LstdFlags)
			case *lua.LTable:
				logger, err := seelog.LoggerFromConfigAsString(luaToXml(v))
				if err != nil {
					L.RaiseError(err.Error())
				}
				ircobj.Log = log.New(&seelogLogger{logger}, "", 0)
			}
			if n, ok := getNumberField(L, opt, "worker"); ok {
				nworker = int(n)
			}
			if s, ok := getStringField(L, opt, "http"); ok {
				httpaddr = s
			}
			L.Push(luar.New(L, ircobj))
			return 1
		},
		"newlogger": func(L *lua.LState) int {
			logger, err := seelog.LoggerFromConfigAsString(luaToXml(L.CheckTable(1)))
			if err != nil {
				L.RaiseError(err.Error())
			}
			L.Push(luar.New(L, log.New(&seelogLogger{logger}, "", 0)))
			return 1
		},
	})
	L.SetField(mod, "cmain", lua.LChannel(luaMainChan))
	L.SetField(mod, "cworker", lua.LChannel(luaWorkerChan))
	proxyLuar(L, irc.Connection{}, func(L *lua.LState, key string) bool {
		switch key {
		case "on":
			L.Push(L.NewFunction(func(L *lua.LState) int {
				self := L.CheckUserData(1).Value.(*irc.Connection)
				fn := L.CheckFunction(3)
				self.AddCallback(L.CheckString(2), func(e *irc.Event) {
					mutex.Lock()
					defer mutex.Unlock()
					pushN(L, fn, luar.New(L, e))
					L.PCall(1, 0, nil)
				})
				return 0
			}))
		case "serve":
			L.Push(L.NewFunction(func(L *lua.LState) int {
				irc := L.CheckUserData(1).Value.(*irc.Connection)
				v := reflect.ValueOf(*irc)
				for i := 0; i < nworker; i++ {
					irc.Log.Printf("spawn worker\n")
					go func() {
						L := newLuaState(conf)
						for !v.FieldByName("quit").Bool() {
							pushN(L, L.GetGlobal("worker"), <-luaWorkerChan)
							L.PCall(1, 0, nil)
						}
					}()
				}
				if httpaddr != "" {
					server := &http.Server{
						Addr:    httpaddr,
						Handler: &httpHandler{irc, conf},
					}
					irc.Log.Printf("http server started on %s", httpaddr)
					go server.ListenAndServe()
				}

				fn := L.OptFunction(2, L.NewFunction(func(L *lua.LState) int { return 0 }))
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
				return 0
			}))
		default:
			return false

		}
		return true
	})
	proxyLuar(L, log.Logger{}, nullProxy)
	proxyLuar(L, irc.Event{}, nullProxy)
	proxyLuar(L, url.Values{}, nullProxy)
	proxyLuar(L, url.Userinfo{}, nullProxy)
	proxyLuar(L, url.URL{}, nullProxy)

	L.PreloadModule("golbot", func(L *lua.LState) int {
		L.Push(mod)
		return 1
	})

	L.PreloadModule("charset", func(L *lua.LState) int {
		L.Push(L.SetFuncs(L.NewTable(), charsetMod))
		return 1
	})
	luajson.Preload(L)
	L.PreloadModule("re", gluare.Loader)
	L.PreloadModule("http", gluahttp.NewHttpModule(&http.Client{}).Loader)
	L.PreloadModule("sh", gluash.Loader)
	L.PreloadModule("fs", gluafs.Loader)

	if err := L.DoString(`
      local golbot = require("golbot")
      notifywoker = function(msg) golbot.cworker:send(msg) end
	  requestworker = function(msg)
	    msg._result = channel.make()
	    golbot.cworker:send(msg)
		return msg._result:receive()
	  end
      notifymain  = function(msg) golbot.cmain:send(msg) end
	  requestmain = function(msg)
	    msg._result = channel.make()
	    golbot.cmain:send(msg)
		return msg._result:receive()
	  end
	  respond = function(msg, value)
	    if msg and msg._result then
		  msg._result:send(value)
		end
	  end
	`); err != nil {
		panic(err)
	}
	if err := L.DoFile(conf); err != nil {
		panic(err)
	}
	return L
}

func main() {
	var optConfFile string
	flag.Usage = func() {
		fmt.Println(`golbot [-c CONFFILE] COMMAND [COMMAND ARGS...]
General options:
  -c : configuration file path (default: golbot.lua)
Commands:
  run : runs a bot
  init : generate default golbot.lua
`)
	}
	flag.StringVar(&optConfFile, "-c", "golbot.lua", "configuration file path(default: golbot.lua)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 || !(args[0] == "run" || args[0] == "init") {
		flag.Usage()
		os.Exit(1)
	}

	if args[0] == "init" {
		ioutil.WriteFile("golbot.lua", ([]byte)(defaultConfigLua), 0660)
		fmt.Println("./golbot.lua has been generated")
		os.Exit(0)
	}

	luaMainChan = make(chan lua.LValue, 1)
	luaWorkerChan = make(chan lua.LValue, 1)
	mainL := newLuaState(optConfFile)
	mainL.Push(mainL.GetGlobal("main"))
	mainL.Call(0, 0)
}
