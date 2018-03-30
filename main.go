package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/cihub/seelog"
	"github.com/kohkimakimoto/gluafs"
	"github.com/otm/gluash"
	"github.com/robfig/cron"
	"github.com/yuin/gluare"
	"github.com/yuin/gopher-lua"
	luajson "layeh.com/gopher-json"
	"layeh.com/gopher-luar"
)

const defaultConfigLua string = `local golbot = require("golbot")
local json = require("json")
local charset = require("charset")
-- local re = require("re")
-- local requests = require("requests")
-- local sh = require("sh")
-- local fs = require("fs")

function main()
  msglog = golbot.newlogger({"seelog", type="adaptive", mininterval="200000000", maxinterval="1000000000", critmsgcount="5",
      {"formats",
        {"format", id="main", format="%%Date(2006-01-02 15:04:05) %%Msg"},
      },
      {"outputs", formatid="main",
        {"filter", levels="trace,debug,info,warn,error,critical",
          {"console"}
        },
      }
  })

%s

  bot:respond([[\s*(\d+)\s*\+\s*(\d+)\s*]], function(m, e)
    bot:say(e.target, tostring(tonumber(m[2]) + tonumber(m[3])))
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
var logChan chan []interface{}

var mainL *lua.LState
var mutex sync.Mutex

type CronEntry struct {
	Spec     string
	FuncName string
}

type CommonClientOption struct {
	ConfFile string
	HttpAddr string
	Https    struct {
		Addr     string
		CertFile string
		KeyFile  string
	}
	Logger *log.Logger
	Crons  []CronEntry
}

func newCommonClientOption(conf string) *CommonClientOption {
	return &CommonClientOption{
		ConfFile: conf,
		HttpAddr: "",
		Logger:   nil,
	}
}

type cronJob struct {
	jobName string
	logger  *log.Logger
	conf    string
}

func (cj *cronJob) Run() {
	cj.logger.Printf("[INFO] cron '%s' started", cj.jobName)
	L := newLuaState(cj.conf)
	defer L.Close()
	L.Push(L.GetGlobal(cj.jobName))
	if err := L.PCall(0, 0, nil); err != nil {
		cj.logger.Printf("[ERROR] cron '%s' : %s", cj.jobName, err.Error())
	} else {
		cj.logger.Printf("[INFO] cron '%s' successfully completed", cj.jobName)
	}
}

func startCrons(co *CommonClientOption) {
	if co.Crons == nil {
		return
	}
	c := cron.New()
	c.ErrorLog = co.Logger
	for _, entry := range co.Crons {
		c.AddJob(entry.Spec, &cronJob{entry.FuncName, co.Logger, co.ConfFile})
	}
	c.Start()
}

func startLog(co *CommonClientOption) {
	go func() {
		for {
			select {
			case v := <-logChan:
				co.Logger.Printf(v[0].(string), v[1:]...)
			}
		}
	}()
}

func startHttpServer(co *CommonClientOption) {
	if co.HttpAddr != "" {
		server := &http.Server{
			Addr:    co.HttpAddr,
			Handler: &httpHandler{co.Logger, co.ConfFile, false},
		}
		co.Logger.Printf("http server started on %s", co.HttpAddr)
		go func() {
			if err := server.ListenAndServe(); err != nil {
				co.Logger.Printf("[ERROR] http server:%s", err.Error())
			}
		}()
	}

	if co.Https.Addr != "" {
		server := &http.Server{
			Addr:    co.Https.Addr,
			Handler: &httpHandler{co.Logger, co.ConfFile, true},
		}
		co.Logger.Printf("https server started on %s(cert:%s, key:%s)", co.Https.Addr, co.Https.CertFile, co.Https.KeyFile)
		go func() {
			if err := server.ListenAndServeTLS(co.Https.CertFile, co.Https.KeyFile); err != nil {
				co.Logger.Printf("[ERROR] https server: %s", err.Error())
			}
		}()
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
	isTLS  bool
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	protocol := "http"
	if h.isTLS {
		protocol = "https"
	}
	h.logger.Printf("[INFO] %s %s %s %s %s ", protocol, r.RemoteAddr, r.Method, r.RequestURI, r.Proto)
	L := newLuaState(h.conf)
	defer L.Close()
	pushN(L, L.GetGlobal(protocol), luar.New(L, r))
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
	luar.GetConfig(L).FieldNames = func(s reflect.Type, f reflect.StructField) []string {
		return []string{toSnakeCase(f.Name)}
	}
	luar.GetConfig(L).MethodNames = func(s reflect.Type, m reflect.Method) []string {
		return []string{toSnakeCase(m.Name)}
	}

	registerIRCChatClientType(L)
	registerSlackChatClientType(L)
	registerHipchatChatClientType(L)
	registerNullChatClientType(L)
	registerRocketChatClientType(L)
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
			if s, ok := getStringField(L, opt, "http"); ok {
				co.HttpAddr = s
			}
			if tbl, ok := L.GetField(opt, "https").(*lua.LTable); ok {
				if s, ok := getStringField(L, tbl, "addr"); ok {
					co.Https.Addr = s
				}
				if s, ok := getStringField(L, tbl, "cert"); ok {
					co.Https.CertFile = s
				}
				if s, ok := getStringField(L, tbl, "key"); ok {
					co.Https.KeyFile = s
				}
			}
			if tbl, ok := L.GetField(opt, "crons").(*lua.LTable); ok {
				co.Crons = []CronEntry{}
				tbl.ForEach(func(key, value lua.LValue) {
					entry := value.(*lua.LTable)
					co.Crons = append(co.Crons, CronEntry{entry.RawGetInt(1).String(), entry.RawGetInt(2).String()})
				})
			}

			switch L.CheckString(1) {
			case "IRC":
				newIRCChatClient(L, co, opt)
			case "Slack":
				newSlackChatClient(L, co, opt)
			case "Hipchat":
				newHipchatChatClient(L, co, opt)
			case "Null":
				newNullChatClient(L, co, opt)
			case "Rocket":
				newRocketChatClient(L, co, opt)
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
	addLuaMethod(L, &http.Request{}, func(L *lua.LState, key string) bool {
		if key == "readbody" || key == "ReadBody" {
			L.Push(L.NewFunction(func(L *lua.LState) int {
				r := L.CheckUserData(1).Value.(*http.Request)
				b, err := ioutil.ReadAll(r.Body)
				defer r.Body.Close()
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
	L.PreloadModule("requests", func(L *lua.LState) int {
		L.Push(L.SetFuncs(L.NewTable(), requestsMod))
		return 1
	})
	luajson.Preload(L)
	L.PreloadModule("re", gluare.Loader)
	L.PreloadModule("sh", gluash.Loader)
	L.PreloadModule("fs", gluafs.Loader)
	L.SetGlobal("goworker", L.NewFunction(func(L *lua.LState) int {
		go func() {
			L := newLuaState(conf)
			pushN(L, L.GetGlobal("worker"), <-luaWorkerChan)
			if err := L.PCall(1, 0, nil); err != nil {
				logChan <- []interface{}{"[ERROR] %s", err.Error()}
			}
		}()
		luaWorkerChan <- L.CheckAny(1)
		return 0
	}))

	if err := L.DoString(`
      local golbot = require("golbot")
      local requests = require("requests")
      local json = require("json")
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
      requests.json = function(opt)
	    local headers = opt.headers or {}
		opt.headers = headers
		local found = false
		for i, v in ipairs(headers) do
		    if i%2 == 0 and string.lower(v) == "content-type" then
			  found = true
			  break
			end
		end
		if not found then
		  table.insert(headers, "Content-Type")
		  table.insert(headers, "application/json")
		end
        jdata, e = json.encode(opt.json)
		if jdata == nil then
		  return jdata, e
		end
		opt.data = jdata
		body, resp = requests.request(opt)
		if body == nil then
		  return body, resp
		end
		return json.decode(body), resp
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
  init : generate default golbot.lua.
      init irc : for IRC
      init slack : for Slack
      init hipchat : for Hipchat
      init rocket : for RocketChat
      init null : empty bot
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
		if len(args) < 2 {
			flag.Usage()
			os.Exit(1)
		}
		switch args[1] {
		case "irc":
			ioutil.WriteFile("golbot.lua", ([]byte)(fmt.Sprintf(defaultConfigLua, ircDefaultConfigLua)), 0660)
		case "slack":
			ioutil.WriteFile("golbot.lua", ([]byte)(fmt.Sprintf(defaultConfigLua, slackDefaultConfigLua)), 0660)
		case "hipchat":
			ioutil.WriteFile("golbot.lua", ([]byte)(fmt.Sprintf(defaultConfigLua, hipchatDefaultConfigLua)), 0660)
		case "rocket":
			ioutil.WriteFile("golbot.lua", ([]byte)(fmt.Sprintf(defaultConfigLua, rocketDefaultConfigLua)), 0660)
		case "null":
			ioutil.WriteFile("golbot.lua", ([]byte)(fmt.Sprintf(defaultConfigLua, nullDefaultConfigLua)), 0660)
		default:
			flag.Usage()
			os.Exit(1)
		}
		fmt.Println("./golbot.lua has been generated")
		os.Exit(0)
	}

	luaMainChan = make(chan lua.LValue)
	luaWorkerChan = make(chan lua.LValue)
	logChan = make(chan []interface{})
	mainL := newLuaState(optConfFile)
	mainL.Push(mainL.GetGlobal("main"))
	mainL.Call(0, 0)
}
