## golbot - A Lua scriptable chat bot

golbot is a Lua scriptable chat bot written in Go. 

## Supported procotols

- IRC

## Install

```bash
go get github.com/yuin/golbot
```

## Getting started

```bash
golbot init
golbot run
```

## Scripting with Lua

Here is a default config script :

```lua
local golbot = require("golbot")                           -- 1
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

  local bot = golbot.newbot("IRC", mynick, myname, {       -- 2
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

  assert(bot:connect("localhost:6667,#test"))              -- 3

  bot:respond([[\s*(\d+)\s*\+\s*(\d+)\s*]], function(m, e) -- 4
    bot:say(e.target, tostring(tonumber(m[2]) + tonumber(m[3])))
  end)

  bot:on("PRIVMSG", function(e)                            -- 5
    local ch = e.arguments[1]
    local nick = e.nick
    local user = e.user
    local source = e.source
    local msg = e:message()
    if nick == mynick then
      return
    end

    msglog:printf("%s\t%s\t%s", ch, source, msg)
    bot.raw:privmsg(ch, msg)                               -- 6
    notifywoker({channel=ch, message=msg, nick=nick})      -- 7
  end)

  bot:serve(function(msg)                                  -- 8
    if msg.type == "say" then
      bot:say(msg.channel, msg.message)
      respond(msg, true)                                   -- 9
    end
  end)
end

function worker(msg)                                       -- 10
  notifymain({type="say", channel=msg.channel, message="accepted"}) -- 11
end

function http(r)                                           -- 12
  if r.method == "POST" and r.URL.path == "/say" then
    local msg = json.decode(r:readbody())
    local ok, success = requestmain({type="say", channel=msg.channel, message=msg.message, result=result}) -- 13
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
```

- 1. requires golbot library.
- 2. creates new bot.
    - `#1` : chat type. currently supports only `"IRC"`
    - `#2` : nick name
    - `#3` : user name
    - `#4` : options(including protocol specific) as a table 
        - Common options are:
            - `worker` : number of worker goroutines
            - `log` : 
                - `function` : function to log system messages( `function(msg:string) end` )
                - `table` : [seelog](https://github.com/cihub/seelog) XML configuration as a lua table to log system messages
            - `http` : Address with port for binding HTTP REST API server
        - `userTLS` and `password` are IRC specific options
- 3. connects to the server.
    - `#1` : procotol specific connection spec
- 4. adds a callback that will be called when bot receives a message.
    - `#1` : regular expression(this value will be evaluated by Go's regexp package)
    - `#2` : callback function
        - `m(table)` : captured groups as a list of strings.
        - `e(object)` : message event object:
            - `target(string)`
            - `from(string)`
            - `message(string)`
            - `raw(object)`: underlying procotol specific object
- 5. adds a callback for procotol specific events.
    - `#1` : event name
    - `#2` : callback function
        - `e(object)` : procotol specific event object
- 6. calls underlying procotol specific client methods.
- 7. sends a message to the worker goroutines.
- 8. starts main goroutine.
    - `#1` : callback function that will be called when messages are sent by worker goroutines.  The callback function that will be called when messages are sent by main gorougine.
- 9. responds to the message from other goroutines.
- 10. a function that will be executed in worker goroutines.
- 11. sends a message to the main goroutine.
- 12. a function that will be executed when http requests are arrived.
- 13. sends a message to the main goroutine and receives a result from the main goroutine.

## IRC 

golbot uses [go-ircevent](https://github.com/thoj/go-ircevent) as an IRC client, [GopherLua](https://github.com/yuin/gopher-lua) as a Lua script runtime, and [gopher-luar](https://github.com/layeh/gopher-luar) as a data converter between Go and Lua.

- `golbot.newbot` creates new `*irc.Connection` (in `go-ircevent`)  object wrapped by gopher-luar, so `bot.raw` has same methods as `*irc.Connection` .
- Protocol specific event object has same method as `*irc.Event`
- Protocol specific event names are same as first argument of `*irc.Connection#AddCallback` .
- Protocol specific options for `golbot.newbot` are:
    - `useTLS(bool)`
    - `password(string)`


## Logging

golbot is integrated with [seelog](https://github.com/cihub/seelog) . `golbot.newlog(tbl)` creates a new logger that has a `printf` method.

```lua
  log = golbot.newlog(conf)
  log:printf("[info] msg")
```

First word surrounded by `[]` is a log level in seelog. Rest are actual messages.

## Working with non-UTF8 servers

`charset` module provides character set conversion functions.

- `charset.encode(string, charset)` : converts `string` from utf-8 to `charset` .
- `charset.decode(string, charset)` : converts `string` from `charset` to utf-8 .

Examples:

```lua
bot:say("#ch", charset.encode("こんにちわ", "iso-2022-jp"))

bot:respond(".*", function(m, e)
  bot.log:printf("%s", charset.decode(e.message, "iso-2022-jp"))
end)
```

## Groutine model

golbot consists of multiple goroutines.

- main goroutine : connects and communicates with IRC servers.
- worker goroutines : typically execute function may takes a long time.
- http goroutines : handle REST API requests.

Consider, for example, the case of deploying web applications when receives a special message.

```lua
  bot:respond("do_deploy", function(m, e)
     do_deploy()
  end)
```

`do_deploy()` may takes a minute, main goroutine is locked all the time. For avoiding this, `do_deploy` should be executed in worker goroutines via messaging(internally, golbot use Go channels).

```lua
function main()
  -- blah blah
  bot:respond("do_deploy", function(m, e)
     notifywoker({ch=e.target, message="do_deploy"})
     bot:say(e.target, "Your deploy request is accepted. Please wait a minute.")
  end)

  bot:serve(function(msg)
    bot:say(msg.ch, msg.message)
  end)
end

function worker(msg)
  if msg.message == "do_deploy" then
    do_deploy()
    notifymain({ch=msg.ch, message="do_deploy successfully completed!"})
  end
end
```

`golbot` provides functions that simplify communications between goroutines through channels.

- notifywoker(msg:table) : sends the `msg` to worker goroutines.
- requestworker(msg:table) : sends the `msg` to worker goroutines and receives a result from worker goroutines.
- notifymain(msg:table) : sends the `msg` to the main goroutine.
- requestmain(msg:table) : sends the `msg` to the main goroutine and receives a result from the main goroutine.
- respond(requestmsg:table, result:any) : sends the `result` to the requestor.

## Create REST API

If the `http` global function exists in the `golbot.lua`, REST API feature will be enabled.

```lua
function http(r)
  if r.method == "POST" and r.URL.path == "/say" then
    local msg = json.decode(r:readbody())
    local ok, success = requestmain({type="say", channel=msg.channel, message=msg.message, result=result})
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
```

`http` function receives `net/http#Request` object wrapped by `gopher-luar` .

`http` function must return 3 objects:

- HTTP status:number : HTTP status code.
- headers:table : a table contains response headers.
- contents:string : response body.

`http` function will be executed in its own thread. You can use `notify*` and `request*` functions for communicating with other goroutines.

## Bundled Lua libraries

- [gluare](https://github.com/yuin/gluare>)
- [gluahttp](https://github.com/cjoudrey/gluahttp)
- [gopher-json](https://github.com/layeh/gopher-json)
- [glusfs](https://github.com/kohkimakimoto/gluafs>)
- [gluash](https://github.com/otm/gluash)

## Author

Yusuke Inuzuka

## License

[BSD License](http://opensource.org/licenses/BSD-2-Clause)

