package main

import (
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/layeh/gopher-luar"
	"github.com/nlopes/slack"
	"github.com/yuin/gopher-lua"
)

const slackChatClientTypeName = "slackChatClient"
const slackDefaultConfigLua = `  myname = "golbot"
  local bot = golbot.newbot("Slack", {
    token = "",
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
    local ch = e.channel
    local user = e.user
    local msg = e.text

    if e.sub_type == "" then
      msglog:printf("%s\t%s\t%s", ch, user, msg)
      bot:say(ch, msg)
      goworker({channel=ch, message=msg, nick=nick})
	end
  end)


  bot:respond([[\s*(\d+)\s*\+\s*(\d+)\s*]], function(m, e)
    bot:say(e.target, tostring(tonumber(m[2]) + tonumber(m[3])))
  end)
`

type slackChatClient struct {
	slackobj       *slack.Client
	rtm            *slack.RTM
	commonOption   *CommonClientOption
	logger         *log.Logger
	startedAt      float64
	callbacks      map[string][]*lua.LFunction
	userId2Name    map[string]string
	userName2Id    map[string]string
	channelId2Name map[string]string
	channelName2Id map[string]string
}

func (client *slackChatClient) toSlackChannelId(v string) string {
	if ok, _ := regexp.MatchString(`[CU][0-9].*`, v); ok {
		return v
	}
	if strings.HasPrefix(v, "#") {
		if v, ok := client.channelName2Id[v[1:]]; ok {
			return v
		}
	}
	if v, ok := client.userName2Id[v]; ok {
		return v
	}
	return v
}

func (client *slackChatClient) applyCallback(L *lua.LState, msg *slack.RTMEvent) {
	mutex.Lock()
	defer mutex.Unlock()
	v, ok := client.callbacks[msg.Type]
	if !ok {
		return
	}
	for _, callback := range v {
		if e, ok := msg.Data.(*slack.MessageEvent); ok {
			f, _ := strconv.ParseFloat(e.Timestamp, 64)
			if (f - client.startedAt) < 3 {
				return
			}
		}
		pushN(L, callback, luar.New(L, msg.Data))
		if err := L.PCall(1, 0, nil); err != nil {
			client.logger.Printf("[Error] %s", err.Error())
		}
	}
}

func (client *slackChatClient) Logger() *log.Logger {
	return client.logger
}

func (client *slackChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *slackChatClient) Say(target, message string) {
	client.rtm.SendMessage(client.rtm.NewOutgoingMessage(message, client.toSlackChannelId(target)))
}

func (client *slackChatClient) On(L *lua.LState, typ string, callback *lua.LFunction) {
	mutex.Lock()
	defer mutex.Unlock()
	v, ok := client.callbacks[typ]
	if !ok {
		v = []*lua.LFunction{}
		client.callbacks[typ] = v
	}
	client.callbacks[typ] = append(v, callback)
}

func (client *slackChatClient) Respond(L *lua.LState, pattern string, fn *lua.LFunction) {
	re := regexp.MustCompile(pattern)
	client.On(L, "message", L.NewFunction(func(L *lua.LState) int {
		e := L.CheckUserData(1).Value.(*slack.MessageEvent)
		matches := re.FindAllStringSubmatch(e.Text, -1)
		if (e.SubType == "me_message" || len(e.SubType) == 0) && len(matches) != 0 {
			user := client.userId2Name[e.User]
			channel := "#" + client.channelId2Name[e.Channel]
			pushN(L, fn, luar.New(L, matches[0]), luar.New(L, NewMessageEvent(user, channel, e.Text, e)))
			if err := L.PCall(2, 0, nil); err != nil {
				client.logger.Printf("[ERROR] %s", err.Error())
			}
		}
		return 0
	}))
}

func (client *slackChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	rtm := client.rtm
	client.startedAt = float64(time.Now().Unix())
	go rtm.ManageConnection()

	for {
		select {
		case msg := <-rtm.IncomingEvents:
			client.applyCallback(L, &msg)
			switch ev := msg.Data.(type) {
			case *slack.ChannelCreatedEvent:
				client.logger.Printf("[Info] Channel created : %s(ID:%s)", ev.Channel.Name, ev.Channel.ID)
				client.channelName2Id[ev.Channel.Name] = ev.Channel.ID
				client.channelId2Name[ev.Channel.ID] = ev.Channel.Name
			case *slack.ChannelRenameEvent:
				client.logger.Printf("[Info] Channel renamed: ID:%s %s -> %s", ev.Channel.ID, client.channelId2Name[ev.Channel.ID], ev.Channel.Name)
				delete(client.channelName2Id, client.channelId2Name[ev.Channel.ID])
				client.channelName2Id[ev.Channel.Name] = ev.Channel.ID
			case *slack.ChannelDeletedEvent:
				client.logger.Printf("[Info] Channel deleted: %s(ID:%s)", client.channelId2Name[ev.Channel], ev.Channel)
				delete(client.channelName2Id, client.channelId2Name[ev.Channel])
				delete(client.channelId2Name, ev.Channel)

			case *slack.ConnectedEvent:
				channels := []string{}
				for _, c := range ev.Info.Channels {
					client.channelName2Id[c.Name] = c.ID
					client.channelId2Name[c.ID] = c.Name
					channels = append(channels, c.Name)
				}
				for _, u := range ev.Info.Users {
					client.userName2Id[u.Name] = u.ID
					client.userId2Name[u.ID] = u.Name
				}
				client.logger.Printf("[Info] Connected to %s(channels:%s)", ev.Info.Team.Domain, strings.Join(channels, ","))
			default:
				// Ignore other events..
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

func registerSlackChatClientType(L *lua.LState) {
	registerChatClientType(L, slackChatClientTypeName)
	proxyLuar(L, slack.RTM{}, nil)
	proxyLuar(L, slack.Client{}, nil)
	for _, v := range eventMapping {
		proxyLuar(L, v, nil)
	}
}

func newSlackChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	token, ok := getStringField(L, opt, "token")
	if !ok {
		L.RaiseError("'token' is required")
	}

	slackobj := slack.New(token)
	chatClient := &slackChatClient{slackobj, nil, co, co.Logger, 0, make(map[string][]*lua.LFunction), make(map[string]string), make(map[string]string), make(map[string]string), make(map[string]string)}

	if co.Logger == nil {
		slack.SetLogger(log.New(os.Stdout, "", log.Lshortfile|log.LstdFlags))
	} else {
		slack.SetLogger(co.Logger)
	}

	chatClient.rtm = chatClient.slackobj.NewRTM()
	L.Push(newChatClient(L, slackChatClientTypeName, chatClient, luar.New(L, chatClient.rtm).(*lua.LUserData)))
}

var eventMapping = map[string]interface{}{ // {{{
	"message":         slack.MessageEvent{},
	"presence_change": slack.PresenceChangeEvent{},
	"user_typing":     slack.UserTypingEvent{},

	"channel_marked":          slack.ChannelMarkedEvent{},
	"channel_created":         slack.ChannelCreatedEvent{},
	"channel_joined":          slack.ChannelJoinedEvent{},
	"channel_left":            slack.ChannelLeftEvent{},
	"channel_deleted":         slack.ChannelDeletedEvent{},
	"channel_rename":          slack.ChannelRenameEvent{},
	"channel_archive":         slack.ChannelArchiveEvent{},
	"channel_unarchive":       slack.ChannelUnarchiveEvent{},
	"channel_history_changed": slack.ChannelHistoryChangedEvent{},

	"dnd_updated":      slack.DNDUpdatedEvent{},
	"dnd_updated_user": slack.DNDUpdatedEvent{},

	"im_created":         slack.IMCreatedEvent{},
	"im_open":            slack.IMOpenEvent{},
	"im_close":           slack.IMCloseEvent{},
	"im_marked":          slack.IMMarkedEvent{},
	"im_history_changed": slack.IMHistoryChangedEvent{},

	"group_marked":          slack.GroupMarkedEvent{},
	"group_open":            slack.GroupOpenEvent{},
	"group_joined":          slack.GroupJoinedEvent{},
	"group_left":            slack.GroupLeftEvent{},
	"group_close":           slack.GroupCloseEvent{},
	"group_rename":          slack.GroupRenameEvent{},
	"group_archive":         slack.GroupArchiveEvent{},
	"group_unarchive":       slack.GroupUnarchiveEvent{},
	"group_history_changed": slack.GroupHistoryChangedEvent{},

	"file_created":         slack.FileCreatedEvent{},
	"file_shared":          slack.FileSharedEvent{},
	"file_unshared":        slack.FileUnsharedEvent{},
	"file_public":          slack.FilePublicEvent{},
	"file_private":         slack.FilePrivateEvent{},
	"file_change":          slack.FileChangeEvent{},
	"file_deleted":         slack.FileDeletedEvent{},
	"file_comment_added":   slack.FileCommentAddedEvent{},
	"file_comment_edited":  slack.FileCommentEditedEvent{},
	"file_comment_deleted": slack.FileCommentDeletedEvent{},

	"pin_added":   slack.PinAddedEvent{},
	"pin_removed": slack.PinRemovedEvent{},

	"star_added":   slack.StarAddedEvent{},
	"star_removed": slack.StarRemovedEvent{},

	"reaction_added":   slack.ReactionAddedEvent{},
	"reaction_removed": slack.ReactionRemovedEvent{},

	"pref_change": slack.PrefChangeEvent{},

	"team_join":              slack.TeamJoinEvent{},
	"team_rename":            slack.TeamRenameEvent{},
	"team_pref_change":       slack.TeamPrefChangeEvent{},
	"team_domain_change":     slack.TeamDomainChangeEvent{},
	"team_migration_started": slack.TeamMigrationStartedEvent{},

	"manual_presence_change": slack.ManualPresenceChangeEvent{},

	"user_change": slack.UserChangeEvent{},

	"emoji_changed": slack.EmojiChangedEvent{},

	"commands_changed": slack.CommandsChangedEvent{},

	"email_domain_changed": slack.EmailDomainChangedEvent{},

	"bot_added":   slack.BotAddedEvent{},
	"bot_changed": slack.BotChangedEvent{},

	"accounts_changed": slack.AccountsChangedEvent{},

	"reconnect_url": slack.ReconnectUrlEvent{},
} // }}}
