package ircserver

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/sorcix/irc.v2"
)

var (
	captchaChallengesSent = prometheus.NewCounter(
		prometheus.CounterOpts{
			Subsystem: "captcha",
			Name:      "challenges_sent",
			Help:      "Number of CAPTCHA challenges generated and sent to users",
		},
	)

	Commands = make(map[string]*ircCommand)
)

type ircCommand struct {
	Func func(*IRCServer, *Session, *Replyctx, *irc.Message)

	// MinParams ensures that enough parameters were specified.
	// irc.ERR_NEEDMOREPARAMS is returned in case less than MinParams
	// parameters were found, otherwise, Func is called.
	MinParams int
}

func init() {
	prometheus.MustRegister(captchaChallengesSent)

	// Keep this list ordered the same way the functions below are ordered.
	Commands["PING"] = &ircCommand{
		Func: (*IRCServer).cmdPing,
	}
	Commands["NICK"] = &ircCommand{
		Func: (*IRCServer).cmdNick,
	}
	Commands["USER"] = &ircCommand{
		Func:      (*IRCServer).cmdUser,
		MinParams: 3,
	}
	Commands["JOIN"] = &ircCommand{
		Func:      (*IRCServer).cmdJoin,
		MinParams: 1,
	}
	Commands["PART"] = &ircCommand{
		Func:      (*IRCServer).cmdPart,
		MinParams: 1,
	}
	Commands["KICK"] = &ircCommand{
		Func:      (*IRCServer).cmdKick,
		MinParams: 2,
	}
	Commands["QUIT"] = &ircCommand{
		Func: (*IRCServer).cmdQuit,
	}
	Commands["PRIVMSG"] = &ircCommand{
		Func: (*IRCServer).cmdPrivmsg,
	}
	Commands["NOTICE"] = &ircCommand{
		Func: (*IRCServer).cmdPrivmsg,
	}
	Commands["MODE"] = &ircCommand{
		Func:      (*IRCServer).cmdMode,
		MinParams: 1,
	}
	Commands["WHO"] = &ircCommand{
		Func: (*IRCServer).cmdWho,
	}
	Commands["OPER"] = &ircCommand{Func: (*IRCServer).cmdOper, MinParams: 2}
	Commands["KILL"] = &ircCommand{
		Func:      (*IRCServer).cmdKill,
		MinParams: 1,
	}
	Commands["AWAY"] = &ircCommand{
		Func: (*IRCServer).cmdAway,
	}
	Commands["TOPIC"] = &ircCommand{
		Func:      (*IRCServer).cmdTopic,
		MinParams: 1,
	}
	Commands["MOTD"] = &ircCommand{
		Func: (*IRCServer).cmdMotd,
	}
	Commands["WHOIS"] = &ircCommand{
		Func:      (*IRCServer).cmdWhois,
		MinParams: 1,
	}
	Commands["LIST"] = &ircCommand{
		Func: (*IRCServer).cmdList,
	}
	Commands["INVITE"] = &ircCommand{
		Func:      (*IRCServer).cmdInvite,
		MinParams: 2,
	}
	Commands["USERHOST"] = &ircCommand{
		Func:      (*IRCServer).cmdUserhost,
		MinParams: 1,
	}
	Commands["NAMES"] = &ircCommand{
		Func: (*IRCServer).cmdNames,
	}
	Commands["KNOCK"] = &ircCommand{
		Func:      (*IRCServer).cmdKnock,
		MinParams: 1,
	}
	Commands["ISON"] = &ircCommand{
		Func:      (*IRCServer).cmdIson,
		MinParams: 1,
	}
	serviceAlias := &ircCommand{
		Func: (*IRCServer).cmdServiceAlias,
	}
	Commands["NICKSERV"] = serviceAlias
	Commands["CHANSERV"] = serviceAlias
	Commands["OPERSERV"] = serviceAlias
	Commands["MEMOSERV"] = serviceAlias
	Commands["HOSTSERV"] = serviceAlias
	Commands["BOTSERV"] = serviceAlias
	Commands["NS"] = serviceAlias
	Commands["CS"] = serviceAlias
	Commands["OS"] = serviceAlias
	Commands["MS"] = serviceAlias
	Commands["HS"] = serviceAlias
	Commands["BS"] = serviceAlias

	if os.Getenv("ROBUSTIRC_TESTING_ENABLE_PANIC_COMMAND") == "1" {
		Commands["PANIC"] = &ircCommand{
			Func: func(i *IRCServer, s *Session, reply *Replyctx, msg *irc.Message) {
				panic("PANIC called")
			},
		}
	}
	Commands["PASS"] = &ircCommand{Func: (*IRCServer).cmdPass}
}

func (i *IRCServer) cmdPing(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 1 {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOORIGIN,
			Params:  []string{s.Nick, "No origin specified"},
		})
		return
	}
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.PONG,
		Params:  []string{msg.Params[0]},
	})
}

// login is called by either cmdNick or cmdUser, depending on which message the
// client sends last.
func (i *IRCServer) maybeLogin(s *Session, reply *Replyctx, msg *irc.Message) {
	if s.loggedIn {
		return
	}

	if s.Nick == "" || s.Username == "" {
		return
	}

	if i.captchaRequiredForLogin() {
		captcha := extractPassword(s.Pass, "captcha")
		if err := i.verifyCaptcha(s, captcha); err != nil {
			captchaUrl := i.generateCaptchaURL(s, fmt.Sprintf("login:%d:", s.LastActivity.UnixNano()))
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.NOTICE,
				Params:  []string{s.Nick, "To login, please go to " + captchaUrl},
			})
			captchaChallengesSent.Inc()
			return
		}
	}

	s.loggedIn = true

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_WELCOME,
		Params:  []string{s.Nick, "Welcome to RobustIRC!"},
	})

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_YOURHOST,
		Params:  []string{s.Nick, "Your host is " + i.ServerPrefix.Name},
	})

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_CREATED,
		Params:  []string{s.Nick, "This server was created " + i.ServerCreation.UTC().String()},
	})

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_MYINFO,
		Params:  []string{s.Nick, i.ServerPrefix.Name + " v1 i nstix"},
	})

	// send ISUPPORT as per:
	// http://www.irc.org/tech_docs/draft-brocklesby-irc-isupport-03.txt
	// http://www.irc.org/tech_docs/005.html
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: "005",
		Params: []string{
			"CHANTYPES=#",
			"CHANNELLEN=" + maxChannelLen,
			"NICKLEN=" + maxNickLen,
			"MODES=1",
			"PREFIX=(o)@",
			"KNOCK",
			"are supported by this server",
		},
	})

	i.sendServices(reply, &irc.Message{
		Command: irc.NICK,
		Params: []string{
			s.Nick,
			"1", // hopcount (ignored by anope)
			"1", // timestamp
			s.Username,
			s.ircPrefix.Host,
			i.ServerPrefix.Name,
			s.svid,
			"+",
			s.Realname,
		},
	})

	if pass := extractPassword(s.Pass, "nickserv"); pass != "" {
		i.sendServices(reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: irc.PRIVMSG,
			Params:  []string{"NickServ", fmt.Sprintf("IDENTIFY %s", pass)},
		})
	}

	if pass := extractPassword(s.Pass, "oper"); pass != "" {
		parsed := irc.ParseMessage("OPER " + pass)
		if len(parsed.Params) > 1 {
			i.cmdOper(s, reply, parsed)
		}
	}

	// In the interest of privacy, clear the password to make
	// accidental leaks less likely.
	s.Pass = ""

	i.cmdMotd(s, reply, msg)
}

func (i *IRCServer) cmdNick(s *Session, reply *Replyctx, msg *irc.Message) {
	oldPrefix := s.ircPrefix

	var nick string
	if len(msg.Params) > 0 {
		nick = msg.Params[0]
	}
	if nick == "" {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NONICKNAMEGIVEN,
			Params:  []string{"No nickname given"},
		})
		return
	}

	dest := "*"
	onlyCapsChanged := false // Whether the nick change only changes capitalization.
	if s.loggedIn {
		dest = s.Nick
		onlyCapsChanged = NickToLower(nick) == NickToLower(dest)
	}

	if !IsValidNickname(nick) {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_ERRONEUSNICKNAME,
			Params:  []string{dest, nick, "Erroneous nickname"},
		})
		return
	}

	if _, ok := i.nicks[NickToLower(nick)]; (ok && !onlyCapsChanged) || IsServicesNickname(nick) {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NICKNAMEINUSE,
			Params:  []string{dest, nick, "Nickname is already in use"},
		})
		return
	}

	if hold, ok := i.svsholds[NickToLower(nick)]; ok {
		if !s.LastActivity.After(hold.added.Add(hold.duration)) {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_ERRONEUSNICKNAME,
				Params:  []string{dest, nick, fmt.Sprintf("Erroneous Nickname: %s", hold.reason)},
			})
			return
		}
		// The SVSHOLD expired, so remove it.
		delete(i.svsholds, NickToLower(nick))
	}

	oldNick := NickToLower(s.Nick)
	s.Nick = nick
	i.nicks[NickToLower(s.Nick)] = s
	if oldNick != "" && !onlyCapsChanged {
		delete(i.nicks, oldNick)
		for _, c := range i.channels {
			// Check ok to ensure we never assign the default value (<nil>).
			if modes, ok := c.nicks[oldNick]; ok {
				c.nicks[NickToLower(s.Nick)] = modes
			}
			delete(c.nicks, oldNick)
		}
	}
	s.updateIrcPrefix()

	if oldNick != "" {
		i.sendServices(reply,
			i.sendCommonChannels(s, reply,
				i.sendUser(s, reply, &irc.Message{
					Prefix:  &oldPrefix,
					Command: irc.NICK,
					Params:  []string{nick},
				})))
		return
	}

	i.maybeLogin(s, reply, msg)
}

func (i *IRCServer) cmdUser(s *Session, reply *Replyctx, msg *irc.Message) {
	// We keep the username (so that bans are more effective) and realname
	// (some people actually set it and look at it).
	s.Username = msg.Params[0]
	s.Realname = msg.Trailing()
	s.updateIrcPrefix()
	i.maybeLogin(s, reply, msg)
}

func (i *IRCServer) cmdJoin(s *Session, reply *Replyctx, msg *irc.Message) {
	var keys []string
	if len(msg.Params) > 1 {
		keys = strings.Split(msg.Params[1], ",")
	}
	for idx, channelname := range strings.Split(msg.Params[0], ",") {
		var key string
		if idx <= len(keys)-1 {
			key = keys[idx]
		}
		if !IsValidChannel(channelname) {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{s.Nick, channelname, "No such channel"},
			})
			continue
		}
		var modesmsg *irc.Message
		c, ok := i.channels[ChanToLower(channelname)]
		if !ok {
			if got, limit := uint64(len(i.channels)), i.ChannelLimit(); got >= limit && limit > 0 {
				i.sendUser(s, reply, &irc.Message{
					Prefix:  i.ServerPrefix,
					Command: irc.ERR_NOSUCHCHANNEL,
					Params:  []string{s.Nick, channelname, "No such channel"},
				})
				continue
			}

			c = &channel{
				name:  channelname,
				nicks: make(map[lcNick]*[maxChanMemberStatus]bool),
			}
			c.modes['n'] = true
			c.modes['t'] = true
			modesmsg = &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: "MODE",
				Params:  []string{channelname, "+nt"},
			}
			i.channels[ChanToLower(channelname)] = c
		} else if c.modes['i'] && !s.invitedTo[ChanToLower(channelname)] {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_INVITEONLYCHAN,
				Params:  []string{s.Nick, c.name, "Cannot join channel (+i)"},
			})
			continue
		} else if c.modes['x'] && !s.invitedTo[ChanToLower(channelname)] {
			if err := i.verifyCaptcha(s, key); err != nil {
				captchaUrl := i.generateCaptchaURL(s, fmt.Sprintf("join:%d:%s", s.LastActivity.UnixNano(), c.name))
				i.sendUser(s, reply, &irc.Message{
					Prefix:  i.ServerPrefix,
					Command: irc.NOTICE,
					Params:  []string{s.Nick, "To join " + c.name + ", please go to " + captchaUrl},
				})
				i.sendUser(s, reply, &irc.Message{
					Prefix:  i.ServerPrefix,
					Command: irc.ERR_INVITEONLYCHAN,
					Params:  []string{s.Nick, c.name, "Cannot join channel (+x). Please go to " + captchaUrl},
				})
				captchaChallengesSent.Inc()
				continue
			}
		}
		// Invites are only valid once.
		if c.modes['i'] || c.modes['x'] {
			delete(s.invitedTo, ChanToLower(channelname))
		}
		if _, ok := c.nicks[NickToLower(s.Nick)]; ok {
			continue
		}
		c.nicks[NickToLower(s.Nick)] = &[maxChanMemberStatus]bool{}
		// If the channel did not exist before, the first joining user becomes a
		// channel operator.
		if !ok {
			c.nicks[NickToLower(s.Nick)][chanop] = true
		}
		s.Channels[ChanToLower(channelname)] = true

		i.sendChannel(c, reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: irc.JOIN,
			Params:  []string{channelname},
		})
		if modesmsg != nil {
			i.sendChannel(c, reply, modesmsg)
		}
		var prefix string
		if c.nicks[NickToLower(s.Nick)][chanop] {
			prefix = prefix + string('@')
		}
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: "SJOIN",
			Params:  []string{"1", channelname, prefix + s.Nick},
		})
		// Channel joins integrate the output of MODE, TOPIC and NAMES commands:
		i.cmdMode(s, reply, &irc.Message{Command: irc.MODE, Params: []string{channelname}})
		i.cmdTopic(s, reply, &irc.Message{Command: irc.TOPIC, Params: []string{channelname}})
		i.cmdNames(s, reply, &irc.Message{Command: irc.NAMES, Params: []string{channelname}})
	}
}

func (i *IRCServer) cmdKick(s *Session, reply *Replyctx, msg *irc.Message) {
	channelname := msg.Params[0]
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{s.Nick, channelname, "No such nick/channel"},
		})
		return
	}

	perms, ok := c.nicks[NickToLower(s.Nick)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTONCHANNEL,
			Params:  []string{s.Nick, channelname, "You're not on that channel"},
		})
		return
	}

	if !perms[chanop] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_CHANOPRIVSNEEDED,
			Params:  []string{s.Nick, channelname, "You're not channel operator"},
		})
		return
	}

	if _, ok := c.nicks[NickToLower(msg.Params[1])]; !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_USERNOTINCHANNEL,
			Params:  []string{s.Nick, msg.Params[1], channelname, "They aren't on that channel"},
		})
		return
	}

	// Must exist since c.nicks contains the nick.
	session, _ := i.nicks[NickToLower(msg.Params[1])]

	i.sendServices(reply,
		i.sendChannel(c, reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: irc.KICK,
			Params:  []string{msg.Params[0], msg.Params[1], msg.Trailing()},
		}))

	// TODO(secure): reduce code duplication with cmdPart()
	delete(c.nicks, NickToLower(msg.Params[1]))
	i.maybeDeleteChannel(c)
	delete(session.Channels, ChanToLower(channelname))

}

func (i *IRCServer) cmdPart(s *Session, reply *Replyctx, msg *irc.Message) {
	for _, channelname := range strings.Split(msg.Params[0], ",") {
		c, ok := i.channels[ChanToLower(channelname)]
		if !ok {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{s.Nick, channelname, "No such channel"},
			})
			continue
		}

		if _, ok := c.nicks[NickToLower(s.Nick)]; !ok {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOTONCHANNEL,
				Params:  []string{s.Nick, channelname, "You're not on that channel"},
			})
			continue
		}

		i.sendServices(reply,
			i.sendChannel(c, reply, &irc.Message{
				Prefix:  &s.ircPrefix,
				Command: irc.PART,
				Params:  []string{channelname},
			}))

		delete(c.nicks, NickToLower(s.Nick))
		i.maybeDeleteChannel(c)
		delete(s.Channels, ChanToLower(channelname))

	}
}

func (i *IRCServer) cmdQuit(s *Session, reply *Replyctx, msg *irc.Message) {
	i.DeleteSession(s, reply.msgid)
	if s.loggedIn {
		i.sendServices(reply,
			i.sendCommonChannels(s, reply, &irc.Message{
				Prefix:  &s.ircPrefix,
				Command: irc.QUIT,
				Params:  []string{msg.Trailing()},
			}))
		i.sendUser(s, reply, &irc.Message{
			Command: irc.ERROR,
			Params:  []string{fmt.Sprintf("Closing Link: %s[%s] (%s)", s.Nick, s.ircPrefix.Host, msg.Trailing())},
		})
	}

}

func (i *IRCServer) cmdPrivmsg(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 1 {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NORECIPIENT,
			Params:  []string{s.Nick, "No recipient given (PRIVMSG)"},
		})
		return
	}

	if len(msg.Params) < 2 {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTEXTTOSEND,
			Params:  []string{s.Nick, "No text to send"},
		})
		return
	}

	if strings.HasPrefix(msg.Params[0], "#") {
		c, ok := i.channels[ChanToLower(msg.Params[0])]
		if !ok {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{s.Nick, msg.Params[0], "No such channel"},
			})
			return
		}
		if _, ok := c.nicks[NickToLower(s.Nick)]; !ok && c.modes['n'] {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_CANNOTSENDTOCHAN,
				Params:  []string{s.Nick, c.name, "Cannot send to channel"},
			})
			return
		}
		i.sendChannelButOne(c, s, reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: msg.Command,
			Params:  []string{msg.Params[0], msg.Trailing()},
		})
		return
	}

	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{s.Nick, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	if session.modes['i'] {
		// To message invisible users, you must share a channel with them.
		common := false
		for channelname := range session.Channels {
			if _, ok := s.Channels[channelname]; ok {
				common = true
				break
			}
		}
		if !common {
			return
		}
	}

	i.sendUser(session, reply, &irc.Message{
		Prefix:  &s.ircPrefix,
		Command: msg.Command,
		Params:  []string{msg.Params[0], msg.Trailing()},
	})

	if session.AwayMsg != "" && msg.Command == irc.PRIVMSG {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_AWAY,
			Params:  []string{s.Nick, msg.Params[0], session.AwayMsg},
		})
	}
}

type modeCmd struct {
	Mode  string
	Param string
}

type modeCmds []modeCmd

func (cmds modeCmds) IRCParams() []string {
	var add, remove []modeCmd
	for _, mode := range cmds {
		if mode.Mode[0] == '+' {
			add = append(add, mode)
		} else {
			remove = append(remove, mode)
		}
	}
	var params []string
	var modeStr string
	if len(add) > 0 {
		modeStr = modeStr + "+"
		for _, mode := range add {
			modeStr = modeStr + string(mode.Mode[1])
			if mode.Param != "" {
				params = append(params, mode.Param)
			}
		}
	}
	if len(remove) > 0 {
		modeStr = modeStr + "-"
		for _, mode := range remove {
			modeStr = modeStr + string(mode.Mode[1])
			if mode.Param != "" {
				params = append(params, mode.Param)
			}
		}
	}

	return append([]string{modeStr}, params...)
}

func normalizeModes(msg *irc.Message) []modeCmd {
	if len(msg.Params) <= 1 {
		return nil
	}
	var results []modeCmd
	// true for adding a mode, false for removing it
	adding := true
	modestr := msg.Params[1]
	modearg := 2
	for _, char := range modestr {
		var mode modeCmd
		switch char {
		case '+', '-':
			adding = (char == '+')
		case 'o', 'd':
			// Modes which require a parameter.
			if len(msg.Params) > modearg {
				mode.Param = msg.Params[modearg]
			}
			modearg++
			fallthrough
		default:
			if adding {
				mode.Mode = "+" + string(char)
			} else {
				mode.Mode = "-" + string(char)
			}
		}
		if mode.Mode == "" {
			continue
		}
		results = append(results, mode)
	}
	return results
}

func (i *IRCServer) cmdMode(s *Session, reply *Replyctx, msg *irc.Message) {
	channelname := msg.Params[0]
	// TODO(secure): properly distinguish between users and channels
	if s.Channels[ChanToLower(channelname)] {
		// Channel must exist, the user is in it.
		c := i.channels[ChanToLower(channelname)]
		modes := normalizeModes(msg)
		queryOnly := true

		if len(modes) == 0 {
			modestr := "+"
			for mode := 'A'; mode < 'z'; mode++ {
				if c.modes[mode] {
					modestr += string(mode)
				}
			}
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.RPL_CHANNELMODEIS,
				Params:  []string{s.Nick, channelname, modestr},
			})
			return
		}

		isChanOp := c.nicks[NickToLower(s.Nick)][chanop] || s.Operator

		for _, mode := range modes {
			char := mode.Mode[1]
			if mode.Mode != "+b" {
				// Non-query modes
				queryOnly = false
				if !isChanOp {
					i.sendUser(s, reply, &irc.Message{
						Prefix:  i.ServerPrefix,
						Command: irc.ERR_CHANOPRIVSNEEDED,
						Params:  []string{s.Nick, channelname, "You're not channel operator"},
					})
					return
				}
				newvalue := (mode.Mode[0] == '+')
				switch char {
				case 't', 's', 'i', 'n':
					c.modes[char] = newvalue

				case 'x':
					if i.captchaConfigured() {
						c.modes[char] = newvalue
					} else {
						i.sendUser(s, reply, &irc.Message{
							Prefix:  i.ServerPrefix,
							Command: irc.NOTICE,
							Params:  []string{s.Nick, "Cannot set mode +x, no CaptchaURL/CaptchaHMACSecret configured"},
						})
					}

				case 'o':
					nick := mode.Param
					perms, ok := c.nicks[NickToLower(nick)]
					if !ok {
						i.sendUser(s, reply, &irc.Message{
							Prefix:  i.ServerPrefix,
							Command: irc.ERR_USERNOTINCHANNEL,
							Params:  []string{s.Nick, nick, channelname, "They aren't on that channel"},
						})
					} else {
						// If the user already is a chanop, silently do
						// nothing (like UnrealIRCd).
						if perms[chanop] != newvalue {
							c.nicks[NickToLower(nick)][chanop] = newvalue
						}
					}
				default:
					i.sendUser(s, reply, &irc.Message{
						Prefix:  i.ServerPrefix,
						Command: irc.ERR_UNKNOWNMODE,
						Params:  []string{s.Nick, string(char), "is unknown mode char to me"},
					})
				}
			} else {
				// Query modes
				switch char {
				case 'b':
					i.sendUser(s, reply, &irc.Message{
						Prefix:  i.ServerPrefix,
						Command: irc.RPL_ENDOFBANLIST,
						Params:  []string{s.Nick, channelname, "End of Channel Ban List"},
					})

				default:
					i.sendUser(s, reply, &irc.Message{
						Prefix:  i.ServerPrefix,
						Command: irc.ERR_UNKNOWNMODE,
						Params:  []string{s.Nick, string(char), "is unknown mode char to me"},
					})
				}
			}
		}

		if queryOnly {
			return
		}

		if reply.replyid > 0 {
			// TODO(secure): see how other ircds are handling mixtures of valid/invalid modes. do they sanity check the entire mode string before applying it, or do they keep valid modes while erroring for others?
			return
		}
		i.sendServices(reply,
			i.sendChannel(c, reply, &irc.Message{
				Prefix:  &s.ircPrefix,
				Command: irc.MODE,
				Params:  append([]string{channelname}, modeCmds(modes).IRCParams()...),
			}))
		return
	}

	nick := NickToLower(channelname)
	if session, ok := i.nicks[nick]; ok {
		if nick != NickToLower(s.Nick) &&
			!s.Operator {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_USERSDONTMATCH,
				Params:  []string{s.Nick, "Can't change mode for other users"},
			})
			return
		}
		modes := normalizeModes(msg)

		if len(modes) == 0 {
			modestr := "+"
			for mode := 'A'; mode < 'z'; mode++ {
				if session.modes[mode] {
					modestr += string(mode)
				}
			}
			i.sendServices(reply,
				i.sendUser(s, reply, &irc.Message{
					Prefix:  &s.ircPrefix,
					Command: irc.MODE,
					Params:  []string{session.Nick, modestr},
				}))

		} else {
			for _, mode := range modes {
				char := mode.Mode[1]
				newvalue := (mode.Mode[0] == '+')
				switch char {
				case 'i':
					session.modes[char] = newvalue
				}
			}

			// It would be nice to send the confirmation to s as well
			// (in case s != session), but at least irssi gets
			// confused and applies the mode change to the current
			// user, not the destination user.
			i.sendServices(reply,
				i.sendUser(session, reply, &irc.Message{
					Prefix:  &s.ircPrefix,
					Command: irc.MODE,
					Params:  []string{session.Nick, modeCmds(modes).IRCParams()[0]},
				}))
		}
		return
	}
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.ERR_NOTONCHANNEL,
		Params:  []string{s.Nick, channelname, "You're not on that channel"},
	})
	return
}

func (i *IRCServer) cmdWho(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 1 {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_ENDOFWHO,
			Params:  []string{s.Nick, "End of /WHO list"},
		})
		return
	}

	// TODO: support WHO on nicknames
	channelname := msg.Params[0]

	lastmsg := &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_ENDOFWHO,
		Params:  []string{s.Nick, channelname, "End of /WHO list"},
	}

	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendUser(s, reply, lastmsg)
		return
	}

	if c.modes['s'] {
		if _, ok := c.nicks[NickToLower(s.Nick)]; !ok {
			i.sendUser(s, reply, lastmsg)
			return
		}
	}

	nicks := make([]string, 0, len(c.nicks))
	for nick := range c.nicks {
		nicks = append(nicks, i.nicks[nick].Nick)
	}

	sort.Strings(nicks)

	for _, nick := range nicks {
		session := i.nicks[NickToLower(nick)]
		prefix := session.ircPrefix
		// TODO: also list all other usermodes
		goneStatus := "H"
		if session.AwayMsg != "" {
			goneStatus = "G"
		}
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_WHOREPLY,
			Params:  []string{s.Nick, channelname, prefix.User, prefix.Host, i.ServerPrefix.Name, prefix.Name, goneStatus, "0 " + session.Realname},
		})
	}

	i.sendUser(s, reply, lastmsg)
}

func (i *IRCServer) cmdOper(s *Session, reply *Replyctx, msg *irc.Message) {
	name := msg.Params[0]
	password := msg.Params[1]
	authenticated := false
	for _, op := range i.Config.IRC.Operators {
		if op.Name == name && op.Password == password {
			authenticated = true
			break
		}
	}

	if !authenticated {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_PASSWDMISMATCH,
			Params:  []string{s.Nick, "Password incorrect"},
		})
		return
	}

	s.Operator = true
	s.modes['o'] = true

	modestr := "+"
	for mode := 'A'; mode < 'z'; mode++ {
		if s.modes[mode] {
			modestr += string(mode)
		}
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_YOUREOPER,
		Params:  []string{s.Nick, "You are now an IRC operator"},
	})
	i.sendServices(reply,
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.MODE,
			Params:  []string{s.Nick, modestr},
		}))
}

func (i *IRCServer) cmdKill(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 2 {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NEEDMOREPARAMS,
			Params:  []string{s.Nick, msg.Command, "Not enough parameters"},
		})
		return
	}

	if !s.Operator {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOPRIVILEGES,
			Params:  []string{s.Nick, "Permission Denied - You're not an IRC operator"},
		})
		return
	}

	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{s.Nick, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	i.DeleteSession(session, reply.msgid)

	i.sendServices(reply,
		i.sendCommonChannels(session, reply, &irc.Message{
			Prefix:  &session.ircPrefix,
			Command: irc.QUIT,
			Params:  []string{"Killed by " + s.Nick + ": " + msg.Trailing()},
		}))

	i.sendUser(session, reply, &irc.Message{
		Prefix:  &s.ircPrefix,
		Command: irc.KILL,
		Params:  []string{session.Nick, fmt.Sprintf("ircd!%s!%s (%s)", s.ircPrefix.Host, s.Nick, msg.Trailing())},
	})

	i.sendUser(session, reply, &irc.Message{
		Command: irc.ERROR,
		Params:  []string{fmt.Sprintf("Closing Link: %s[%s] (Killed (%s (%s)))", session.Nick, session.ircPrefix.Host, s.Nick, msg.Trailing())},
	})
}

func (i *IRCServer) cmdAway(s *Session, reply *Replyctx, msg *irc.Message) {
	s.AwayMsg = strings.TrimSpace(msg.Trailing())
	if s.AwayMsg != "" {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_NOWAWAY,
			Params:  []string{s.Nick, "You have been marked as being away"},
		})
		return
	}
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_UNAWAY,
		Params:  []string{s.Nick, "You are no longer marked as being away"},
	})
}

func (i *IRCServer) cmdTopic(s *Session, reply *Replyctx, msg *irc.Message) {
	channel := msg.Params[0]
	c, ok := i.channels[ChanToLower(channel)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{s.Nick, channel, "No such channel"},
		})
		return
	}

	// “TOPIC :”, i.e. unset the topic.
	if msg.Trailing() == "" && len(msg.Params) == 2 {
		if c.modes['t'] && !c.nicks[NickToLower(s.Nick)][chanop] {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_CHANOPRIVSNEEDED,
				Params:  []string{s.Nick, channel, "You're not channel operator"},
			})
			return
		}

		c.topicNick = ""
		c.topicTime = time.Time{}
		c.topic = ""

		i.sendChannel(c, reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: irc.TOPIC,
			Params:  []string{channel, msg.Trailing()},
		})
		i.sendServices(reply, &irc.Message{
			Prefix:  &irc.Prefix{Name: s.Nick},
			Command: irc.TOPIC,
			Params:  []string{channel, s.Nick, "0", msg.Trailing()},
		})
		return
	}

	if !s.Channels[ChanToLower(channel)] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTONCHANNEL,
			Params:  []string{s.Nick, channel, "You're not on that channel"},
		})
		return
	}

	// “TOPIC”, i.e. get the topic.
	if len(msg.Params) == 1 {
		if c.topicTime.IsZero() {
			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.RPL_NOTOPIC,
				Params:  []string{s.Nick, channel, "No topic is set"},
			})
			return
		}

		// TODO(secure): if the channel is secret, return ERR_NOTONCHANNEL

		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_TOPIC,
			Params:  []string{s.Nick, channel, c.topic},
		})
		i.sendUser(s, reply, &irc.Message{
			Prefix: i.ServerPrefix,
			// RPL_TOPICWHOTIME (ircu-specific, not in the RFC)
			Command: "333",
			Params:  []string{s.Nick, channel, c.topicNick, strconv.FormatInt(c.topicTime.Unix(), 10)},
		})
		return
	}

	if c.modes['t'] && !c.nicks[NickToLower(s.Nick)][chanop] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_CHANOPRIVSNEEDED,
			Params:  []string{s.Nick, channel, "You're not channel operator"},
		})
		return
	}

	c.topicNick = s.Nick
	c.topicTime = s.LastActivity
	c.topic = msg.Trailing()

	i.sendChannel(c, reply, &irc.Message{
		Prefix:  &s.ircPrefix,
		Command: irc.TOPIC,
		Params:  []string{channel, msg.Trailing()},
	})
	i.sendServices(reply, &irc.Message{
		Prefix:  &irc.Prefix{Name: s.Nick},
		Command: irc.TOPIC,
		Params:  []string{channel, c.topicNick, strconv.FormatInt(c.topicTime.Unix(), 10), msg.Trailing()},
	})
}

func (i *IRCServer) cmdMotd(s *Session, reply *Replyctx, msg *irc.Message) {
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_MOTDSTART,
		Params:  []string{s.Nick, "- " + i.ServerPrefix.Name + " Message of the day -"},
	})
	// TODO(secure): make motd configurable
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_MOTD,
		Params:  []string{s.Nick, "- No MOTD configured yet."},
	})
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_ENDOFMOTD,
		Params:  []string{s.Nick, "End of MOTD command"},
	})
}

func (i *IRCServer) cmdPass(s *Session, reply *Replyctx, msg *irc.Message) {
	// TODO(secure): document this in the admin/user manual
	// You can specify multiple passwords in a single PASS command, separated
	// by colons and prefixed with <key>=, e.g. “nickserv=secret” or
	// “nickserv=secret:network=letmein” in case the network requires a
	// password _and_ you want to authenticate to nickserv.
	//
	// In case there is no <key>= prefix, nickserv= is added.
	//
	// The valid prefixes are:
	// services= for identifying as a server-to-server connection (services)
	// session= for picking up a saved session (not yet implemented)
	// network= for authenticating to a private network (not yet implemented)
	// nickserv= for authenticating to services
	// oper= for authenticating as an IRC operator
	if len(msg.Params) > 0 {
		s.Pass = strings.Join(msg.Params, " ")
	}
	if !strings.HasPrefix(s.Pass, "nickserv=") &&
		!strings.HasPrefix(s.Pass, "services=") &&
		!strings.HasPrefix(s.Pass, "network=") &&
		!strings.HasPrefix(s.Pass, "oper=") &&
		!strings.HasPrefix(s.Pass, "session=") &&
		!strings.HasPrefix(s.Pass, "captcha=") {
		s.Pass = "nickserv=" + s.Pass
	}

	i.maybeLogin(s, reply, msg)
}

func (i *IRCServer) cmdWhois(s *Session, reply *Replyctx, msg *irc.Message) {
	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{s.Nick, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_WHOISUSER,
		Params:  []string{s.Nick, session.Nick, session.ircPrefix.User, session.ircPrefix.Host, "*", session.Realname},
	})

	var channels []string
	for channel := range session.Channels {
		var prefix string
		c := i.channels[channel]
		if c.modes['s'] && !s.Operator && !s.Channels[channel] {
			continue
		}
		if c.nicks[NickToLower(session.Nick)][chanop] {
			prefix = "@"
		}
		channels = append(channels, prefix+c.name)
	}

	sort.Strings(channels)

	if len(channels) > 0 {
		// TODO(secure): this needs to be split into multiple messages if the line exceeds 510 bytes.
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_WHOISCHANNELS,
			Params:  []string{s.Nick, session.Nick, strings.Join(channels, " ")},
		})
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_WHOISSERVER,
		Params:  []string{s.Nick, session.Nick, i.ServerPrefix.Name, "RobustIRC"},
	})

	if session.Operator {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_WHOISOPERATOR,
			Params:  []string{s.Nick, session.Nick, "is an IRC operator"},
		})
	}

	if session.AwayMsg != "" {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_AWAY,
			Params:  []string{s.Nick, session.Nick, session.AwayMsg},
		})
	}

	idle := strconv.FormatInt(int64(s.LastActivity.Sub(session.LastNonPing).Seconds()), 10)
	signon := strconv.FormatInt(time.Unix(0, session.Id.Id).Unix(), 10)
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_WHOISIDLE,
		Params:  []string{s.Nick, session.Nick, idle, signon, "seconds idle, signon time"},
	})

	if session.modes['r'] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: "307", // RPL_WHOISREGNICK (not in the RFC)
			Params:  []string{s.Nick, session.Nick, "user has identified to services"},
		})
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_ENDOFWHOIS,
		Params:  []string{s.Nick, session.Nick, "End of /WHOIS list"},
	})
}

func (i *IRCServer) cmdList(s *Session, reply *Replyctx, msg *irc.Message) {
	channels := make([]string, 0, len(i.channels))
	if len(msg.Params) > 0 {
		for _, channel := range strings.Split(msg.Params[0], ",") {
			channelname := ChanToLower(strings.TrimSpace(channel))
			if _, ok := i.channels[channelname]; ok {
				channels = append(channels, string(channelname))
			}
		}
	} else {
		for channel := range i.channels {
			channels = append(channels, string(channel))
		}
		sort.Strings(channels)
	}
	for _, channel := range channels {
		c := i.channels[lcChan(channel)]
		if c.modes['s'] && !s.Operator && !s.Channels[lcChan(channel)] {
			continue
		}
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_LIST,
			Params:  []string{s.Nick, c.name, strconv.Itoa(len(c.nicks)), c.topic},
		})
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_LISTEND,
		Params:  []string{s.Nick, "End of LIST"},
	})
}

func (i *IRCServer) cmdInvite(s *Session, reply *Replyctx, msg *irc.Message) {
	nickname := msg.Params[0]
	channelname := msg.Params[1]
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTONCHANNEL,
			Params:  []string{s.Nick, msg.Params[1], "You're not on that channel"},
		})
		return
	}
	if _, ok := c.nicks[NickToLower(s.Nick)]; !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTONCHANNEL,
			Params:  []string{s.Nick, msg.Params[1], "You're not on that channel"},
		})
		return
	}
	session, ok := i.nicks[NickToLower(nickname)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{s.Nick, msg.Params[0], "No such nick/channel"},
		})
		return
	}
	if _, ok := c.nicks[NickToLower(nickname)]; ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_USERONCHANNEL,
			Params:  []string{s.Nick, session.Nick, c.name, "is already on channel"},
		})
		return
	}
	if c.modes['i'] && !c.nicks[NickToLower(s.Nick)][chanop] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_CHANOPRIVSNEEDED,
			Params:  []string{s.Nick, c.name, "You're not channel operator"},
		})
		return
	}
	session.invitedTo[ChanToLower(channelname)] = true
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_INVITING,
		Params:  []string{s.Nick, session.Nick, c.name},
	})
	i.sendServices(reply,
		i.sendUser(session, reply, &irc.Message{
			Prefix:  &s.ircPrefix,
			Command: irc.INVITE,
			Params:  []string{session.Nick, c.name},
		}))
	i.sendChannel(c, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.NOTICE,
		Params:  []string{c.name, fmt.Sprintf("%s invited %s into the channel.", s.Nick, msg.Params[0])},
	})

	if session.AwayMsg != "" {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.RPL_AWAY,
			Params:  []string{s.Nick, msg.Params[0], session.AwayMsg},
		})
	}
}

func (i *IRCServer) cmdUserhost(s *Session, reply *Replyctx, msg *irc.Message) {
	var userhosts []string
	for _, nickname := range msg.Params {
		session, ok := i.nicks[NickToLower(nickname)]
		if !ok {
			continue
		}
		awayPrefix := "+"
		if session.AwayMsg != "" {
			awayPrefix = "-"
		}
		nick := session.Nick
		if session.Operator {
			nick = nick + "*"
		}
		userhosts = append(userhosts, fmt.Sprintf("%s=%s%s", nick, awayPrefix, session.ircPrefix.String()))
	}
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_USERHOST,
		Params:  []string{s.Nick, strings.Join(userhosts, " ")},
	})
}

func (i *IRCServer) cmdServiceAlias(s *Session, reply *Replyctx, msg *irc.Message) {
	aliases := map[string]string{
		"NICKSERV": "PRIVMSG NickServ :",
		"NS":       "PRIVMSG NickServ :",
		"CHANSERV": "PRIVMSG ChanServ :",
		"CS":       "PRIVMSG ChanServ :",
		"OPERSERV": "PRIVMSG OperServ :",
		"OS":       "PRIVMSG OperServ :",
		"MEMOSERV": "PRIVMSG MemoServ :",
		"MS":       "PRIVMSG MemoServ :",
		"HOSTSERV": "PRIVMSG HostServ :",
		"HS":       "PRIVMSG HostServ :",
		"BOTSERV":  "PRIVMSG BotServ :",
		"BS":       "PRIVMSG BotServ :",
	}
	for alias, expanded := range aliases {
		if strings.ToUpper(msg.Command) != alias {
			continue
		}
		i.cmdPrivmsg(s, reply, irc.ParseMessage(expanded+strings.Join(msg.Params, " ")))
		return
	}
}

func (i *IRCServer) cmdNames(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) > 0 {
		channelname := msg.Params[0]
		if c, ok := i.channels[ChanToLower(channelname)]; ok {
			nicks := make([]string, 0, len(c.nicks))
			for nick, perms := range c.nicks {
				var prefix string
				if perms[chanop] {
					prefix = prefix + string('@')
				}
				nicks = append(nicks, prefix+i.nicks[nick].Nick)
			}

			sort.Strings(nicks)

			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.RPL_NAMREPLY,
				Params:  []string{s.Nick, "=", channelname, strings.Join(nicks, " ")},
			})

			i.sendUser(s, reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.RPL_ENDOFNAMES,
				Params:  []string{s.Nick, channelname, "End of /NAMES list."},
			})
			return
		}
	}
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_ENDOFNAMES,
		Params:  []string{s.Nick, "*", "End of /NAMES list."},
	})
}

func (i *IRCServer) cmdKnock(s *Session, reply *Replyctx, msg *irc.Message) {
	channelname := msg.Params[0]
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: "480",
			Params:  []string{s.Nick, fmt.Sprintf("Cannot knock on %s (Channel does not exist)", channelname)},
		})
		return
	}

	if !c.modes['i'] {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: "480",
			Params:  []string{s.Nick, fmt.Sprintf("Cannot knock on %s (Channel is not invite only)", channelname)},
		})
		return
	}

	reason := "no reason specified"
	if len(msg.Params) > 1 {
		reason = strings.Join(msg.Params[1:], " ")
	}

	i.sendChannel(c, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.NOTICE,
		Params:  []string{c.name, fmt.Sprintf("[Knock] by %s (%s)", s.ircPrefix.String(), reason)},
	})
	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.NOTICE,
		Params:  []string{s.Nick, fmt.Sprintf("Knocked on %s", c.name)},
	})
}

func (i *IRCServer) cmdIson(s *Session, reply *Replyctx, msg *irc.Message) {
	var onlineUsers []string
	for _, nickname := range msg.Params {
		if session, ok := i.nicks[NickToLower(nickname)]; ok {
			onlineUsers = append(onlineUsers, session.Nick)
		}
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_ISON,
		Params:  []string{s.Nick, strings.Join(onlineUsers, " ")},
	})
}
