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

  local bot = golbot.newbot("IRC", {
    nickname = mynick,
    username = myname,
    conn = "localhost:6667,#test",
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

  bot:respond([[\s*(\d+)\s*\+\s*(\d+)\s*]], function(m, e)
    bot:say(e.target, tostring(tonumber(m[2]) + tonumber(m[3])))
  end)

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
    notifywoker({channel=ch, message=msg, nick=nick})
  end)

  bot:serve(function(msg)
    if msg.type == "say" then
      bot:say(msg.channel, msg.message)
      respond(msg, true)
    end
  end)
end

function worker(msg)
  notifymain({type="say", channel=msg.channel, message="accepted"})
end

function http(r)
  if r.method == "POST" and r.URL.path == "/say" then
    local msg = json.decode(r:readbody())
    local ok, success = requestmain({type="say", channel=msg.channel, message=msg.message})
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
var luaWorkerQuitChan chan bool

var mainL *lua.LState
var mutex sync.Mutex

type CommonClientOption struct {
	ConfFile   string
	NumWorkers int
	HttpAddr   string
	Logger     *log.Logger
}

func newCommonClientOption(conf string) *CommonClientOption {
	return &CommonClientOption{
		ConfFile:   conf,
		NumWorkers: 3,
		HttpAddr:   "127.0.0.1:6669",
		Logger:     nil,
	}
}

func startHttpServer(co *CommonClientOption) {
	if co.HttpAddr != "" {
		server := &http.Server{
			Addr:    co.HttpAddr,
			Handler: &httpHandler{co.Logger, co.ConfFile},
		}
		co.Logger.Printf("http server started on %s", co.HttpAddr)
		go server.ListenAndServe()
	}
}

func startWorkers(co *CommonClientOption) {
	for i := 0; i < co.NumWorkers; i++ {
		go func() {
			L := newLuaState(co.ConfFile)
			for {
				select {
				case msg := <-luaWorkerChan:
					pushN(L, L.GetGlobal("worker"), msg)
					L.PCall(1, 0, nil)
				case <-luaWorkerQuitChan:
					return
				}
			}
		}()
	}
}

func stopWorkers(co *CommonClientOption) {
	for i := 0; i < co.NumWorkers; i++ {
		luaWorkerQuitChan <- true
	}
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

type httpHandler struct {
	logger *log.Logger
	conf   string
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.logger.Printf("[INFO] HTTP %s %s %s %s ", r.RemoteAddr, r.Method, r.RequestURI, r.Proto)
	L := newLuaState(h.conf)
	defer L.Close()
	pushN(L, L.GetGlobal("http"), luar.New(L, r))
	err := L.PCall(1, 3, nil)
	if err != nil {
		h.logger.Printf("[ERROR] %s", err.Error())
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

	registerIRCChatClientType(L)
	mod := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"newbot": func(L *lua.LState) int {
			opt := L.OptTable(2, L.NewTable())
			co := newCommonClientOption(conf)
			switch v := L.GetField(opt, "log").(type) {
			case *lua.LFunction:
				co.Logger = log.New(&luaLogger{L, v}, "", log.LstdFlags)
			case *lua.LTable:
				l, err := seelog.LoggerFromConfigAsString(luaToXml(v))
				if err != nil {
					L.RaiseError(err.Error())
				}
				co.Logger = log.New(&seelogLogger{l}, "", 0)
			}
			if n, ok := getNumberField(L, opt, "worker"); ok {
				co.NumWorkers = int(n)
			}
			if s, ok := getStringField(L, opt, "http"); ok {
				co.HttpAddr = s
			}

			switch L.CheckString(1) {
			case "IRC":
				newIRCChatClient(L, co, opt)
			default:
				L.RaiseError("unknown chat type: %s", L.ToString(1))
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
	proxyLuar(L, MessageEvent{}, nil)
	proxyLuar(L, log.Logger{}, nil)
	proxyLuar(L, url.Values{}, nil)
	proxyLuar(L, url.Userinfo{}, nil)
	proxyLuar(L, url.URL{}, nil)
	proxyLuar(L, http.Cookie{}, nil)
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

	luaMainChan = make(chan lua.LValue)
	luaWorkerChan = make(chan lua.LValue)
	luaWorkerQuitChan = make(chan bool)
	mainL := newLuaState(optConfFile)
	mainL.Push(mainL.GetGlobal("main"))
	mainL.Call(0, 0)
}
