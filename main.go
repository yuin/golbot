package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/cihub/seelog"
	"github.com/cjoudrey/gluahttp"
	"github.com/kohkimakimoto/gluafs"
	luajson "github.com/layeh/gopher-json"
	"github.com/layeh/gopher-luar"
	"github.com/otm/gluash"
	"github.com/yuin/charsetutil"
	"github.com/yuin/gluare"
	"github.com/yuin/gopher-lua"
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

function http(r)
  if r.method == "POST" and r.URL.path == "/privmsg" then
    local msg = json.decode(r:readbody())
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

type chatClient interface {
	Logger() *log.Logger
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
	client chatClient
	conf   string
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.client.Logger().Printf("[INFO] HTTP %s %s %s %s ", r.RemoteAddr, r.Method, r.RequestURI, r.Proto)
	L := newLuaState(h.conf)
	defer L.Close()
	pushN(L, L.GetGlobal("http"), luar.New(L, r))
	err := L.PCall(1, 3, nil)
	if err != nil {
		h.client.Logger().Printf("[ERROR] %s", err.Error())
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
	var client chatClient
	nworker := 3
	httpaddr := ""

	mod := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"newbot": func(L *lua.LState) int {
			nick := L.CheckString(2)
			user := L.CheckString(3)
			opt := L.OptTable(4, L.NewTable())
			var logger *log.Logger
			switch v := L.GetField(opt, "log").(type) {
			case *lua.LFunction:
				logger = log.New(&luaLogger{L, v}, "", log.LstdFlags)
			case *lua.LTable:
				l, err := seelog.LoggerFromConfigAsString(luaToXml(v))
				if err != nil {
					L.RaiseError(err.Error())
				}
				logger = log.New(&seelogLogger{l}, "", 0)
			}

			switch L.CheckString(1) {
			case "IRC":
				client = newIRCBot(L, nick, user, logger, opt)
			default:
				L.RaiseError("unknown chat type: %s", L.ToString(1))
			}
			if n, ok := getNumberField(L, opt, "worker"); ok {
				nworker = int(n)
			}
			if s, ok := getStringField(L, opt, "http"); ok {
				httpaddr = s
			}
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
	startHttpServer := func() {
		if httpaddr != "" {
			server := &http.Server{
				Addr:    httpaddr,
				Handler: &httpHandler{client, conf},
			}
			client.Logger().Printf("http server started on %s", httpaddr)
			go server.ListenAndServe()
		}
	}
	newIRCLuaState(L, conf, nworker, startHttpServer)

	proxyLuar(L, log.Logger{}, nullProxy)
	proxyLuar(L, url.Values{}, nullProxy)
	proxyLuar(L, url.Userinfo{}, nullProxy)
	proxyLuar(L, url.URL{}, nullProxy)
	proxyLuar(L, http.Cookie{}, nullProxy)
	proxyLuar(L, http.Request{}, func(L *lua.LState, key string) bool {
		if key == "readbody" || key == "ReadBody" {
			L.Push(L.NewFunction(func(L *lua.LState) int {
				r := L.CheckUserData(1).Value.(*http.Request)
				b, err := ioutil.ReadAll(r.Body)
				if err != nil {
					pushN(L, lua.LNil, lua.LString(err.Error()))
					return 2
				}
				pushN(L, lua.LString(b))
				return 1
			}))
			return true
		}
		return false
	})

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
