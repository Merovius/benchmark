package ircserver

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robustirc/robustirc/types"

	"gopkg.in/sorcix/irc.v2"
)

func init() {
	// When developing the anope RobustIRC module, I used the following command
	// to make sure all commands which anope sends are implemented:
	//
	// perl -nlE 'my ($cmd) = ($_ =~ /send_cmd\([^,]+, "([^" ]+)/); say $cmd if defined($cmd)' src/protocol/robustirc.c | sort | uniq

	// NB: This command doesn’t have the server_ prefix because it is sent in
	// order to make a session _become_ a server. Having the function in this
	// file makes more sense than in commands.go.
	Commands["SERVER"] = &ircCommand{Func: (*IRCServer).cmdServer, MinParams: 2}

	// These just use exactly the same code as clients. We can directly assign
	// the contents of Commands[x] because commands.go is sorted lexically
	// before server_commands.go. For details, see
	// http://golang.org/ref/spec#Package_initialization.
	Commands["server_PING"] = Commands["PING"]

	Commands["server_QUIT"] = &ircCommand{
		Func: (*IRCServer).cmdServerQuit,
	}
	Commands["server_NICK"] = &ircCommand{
		Func: (*IRCServer).cmdServerNick,
	}
	Commands["server_MODE"] = &ircCommand{
		Func: (*IRCServer).cmdServerMode,
	}
	Commands["server_JOIN"] = &ircCommand{
		Func: (*IRCServer).cmdServerJoin,
	}
	Commands["server_PART"] = &ircCommand{
		Func: (*IRCServer).cmdServerPart,
	}
	Commands["server_PRIVMSG"] = &ircCommand{
		Func: (*IRCServer).cmdServerPrivmsg,
	}
	Commands["server_NOTICE"] = &ircCommand{
		Func: (*IRCServer).cmdServerPrivmsg,
	}
	Commands["server_TOPIC"] = &ircCommand{
		Func:      (*IRCServer).cmdServerTopic,
		MinParams: 3,
	}
	Commands["server_SVSNICK"] = &ircCommand{
		Func:      (*IRCServer).cmdServerSvsnick,
		MinParams: 2,
	}
	Commands["server_SVSMODE"] = &ircCommand{
		Func:      (*IRCServer).cmdServerSvsmode,
		MinParams: 2,
	}
	Commands["server_SVSHOLD"] = &ircCommand{
		Func:      (*IRCServer).cmdServerSvshold,
		MinParams: 1,
	}
	Commands["server_SVSJOIN"] = &ircCommand{
		Func:      (*IRCServer).cmdServerSvsjoin,
		MinParams: 2,
	}
	Commands["server_SVSPART"] = &ircCommand{
		Func:      (*IRCServer).cmdServerSvspart,
		MinParams: 2,
	}
	Commands["server_KILL"] = &ircCommand{
		Func:      (*IRCServer).cmdServerKill,
		MinParams: 1,
	}
	Commands["server_KICK"] = &ircCommand{
		Func:      (*IRCServer).cmdServerKick,
		MinParams: 2,
	}
	Commands["server_INVITE"] = &ircCommand{
		Func:      (*IRCServer).cmdServerInvite,
		MinParams: 2,
	}
}

func servicesPrefix(prefix *irc.Prefix) *irc.Prefix {
	return &irc.Prefix{
		Name: prefix.Name,
		User: "services",
		Host: "services"}
}

func (i *IRCServer) cmdServerKick(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “:ChanServ KICK #noname-ev blArgh_ :get out”
	channelname := msg.Params[0]
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, channelname, "No such nick/channel"},
		})
		return
	}

	if _, ok := c.nicks[NickToLower(msg.Params[1])]; !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_USERNOTINCHANNEL,
			Params:  []string{msg.Prefix.Name, msg.Params[1], channelname, "They aren't on that channel"},
		})
		return
	}

	// Must exist since c.nicks contains the nick.
	session, _ := i.nicks[NickToLower(msg.Params[1])]

	i.sendServices(reply, i.sendChannel(c, reply, &irc.Message{
		Prefix: &irc.Prefix{
			Name: msg.Prefix.Name,
			User: "services",
			Host: "services",
		},
		Command: irc.KICK,
		Params:  []string{msg.Params[0], msg.Params[1], msg.Trailing()},
	}))

	// TODO(secure): reduce code duplication with cmdPart()
	delete(c.nicks, NickToLower(msg.Params[1]))
	i.maybeDeleteChannel(c)
	delete(session.Channels, ChanToLower(channelname))
}

func (i *IRCServer) cmdServerKill(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 2 {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NEEDMOREPARAMS,
			Params:  []string{"*", msg.Command, "Not enough parameters"},
		})
		return
	}

	killPrefix := msg.Prefix
	for id, session := range i.sessions {
		if id.Id != s.Id.Id || id.Reply == 0 || NickToLower(session.Nick) != NickToLower(msg.Prefix.Name) {
			continue
		}
		killPrefix = &session.ircPrefix
		break
	}

	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{"*", msg.Params[0], "No such nick/channel"},
		})
		return
	}

	killPath := fmt.Sprintf("ircd!%s!%s", killPrefix.Host, killPrefix.Name)
	killPath = strings.Replace(killPath, "!!", "!", -1)

	i.sendUser(session, reply, &irc.Message{
		Prefix:  killPrefix,
		Command: irc.KILL,
		Params:  []string{session.Nick, fmt.Sprintf("%s (%s)", killPath, msg.Trailing())},
	})
	i.sendServices(reply,
		i.sendCommonChannels(session, reply, &irc.Message{
			Prefix:  &session.ircPrefix,
			Command: irc.QUIT,
			Params:  []string{"Killed: " + msg.Trailing()},
		}))
	i.DeleteSession(session, reply.msgid)
}

func (i *IRCServer) cmdServerQuit(s *Session, reply *Replyctx, msg *irc.Message) {
	// No prefix means the server quits the entire session.
	if msg.Prefix == nil {
		i.DeleteSession(s, reply.msgid)
		// For services, we also need to delete all sessions that share the
		// same .Id, but have a different .Reply.
		for id, session := range i.sessions {
			if id.Id != s.Id.Id || id.Reply == 0 {
				continue
			}
			i.sendCommonChannels(session, reply, &irc.Message{
				Prefix:  &session.ircPrefix,
				Command: irc.QUIT,
				Params:  []string{msg.Trailing()},
			})
			i.DeleteSession(session, reply.msgid)
		}
		return
	}

	// We got a prefix, so only a single session quits (e.g. nickname
	// enforcer).
	for id, session := range i.sessions {
		if id.Id != s.Id.Id || id.Reply == 0 || NickToLower(session.Nick) != NickToLower(msg.Prefix.Name) {
			continue
		}
		i.sendCommonChannels(session, reply, &irc.Message{
			Prefix:  &session.ircPrefix,
			Command: irc.QUIT,
			Params:  []string{msg.Trailing()},
		})
		i.DeleteSession(session, reply.msgid)
		return
	}
}

func (i *IRCServer) cmdServerNick(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “NICK OperServ 1 1422134861 services localhost.net services.localhost.net 0 :Operator Server”
	// <nickname> <hopcount> <username> <host> <servertoken> <umode> <realname>

	// Could be either a nickchange or the introduction of a new user.
	if len(msg.Params) == 1 {
		// TODO(secure): handle nickchanges. not sure when/if those are used. botserv maybe?
		return
	}

	if _, ok := i.nicks[NickToLower(msg.Params[0])]; ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NICKNAMEINUSE,
			Params:  []string{"*", msg.Params[0], "Nickname is already in use"},
		})
		return
	}

	h := fnv.New64()
	h.Write([]byte(msg.Params[0]))
	id := types.RobustId{
		Id:    s.Id.Id,
		Reply: int64(h.Sum64()),
	}

	i.CreateSession(id, "")
	ss := i.sessions[id]
	ss.Nick = msg.Params[0]
	i.nicks[NickToLower(ss.Nick)] = ss
	ss.Username = msg.Params[3]
	ss.Realname = msg.Trailing()
	ss.updateIrcPrefix()
}

func (i *IRCServer) cmdServerMode(s *Session, reply *Replyctx, msg *irc.Message) {
	channelname := msg.Params[0]
	// TODO(secure): properly distinguish between users and channels
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	// TODO(secure): possibly refactor this with cmdMode()
	modes := normalizeModes(msg)
	for _, mode := range modes {
		char := mode.Mode[1]
		newvalue := (mode.Mode[0] == '+')

		switch char {
		case 't', 's', 'r', 'i':
			c.modes[char] = newvalue
		case 'o':
			nick := mode.Param
			perms, ok := c.nicks[NickToLower(nick)]
			if !ok {
				i.sendServices(reply, &irc.Message{
					Prefix:  i.ServerPrefix,
					Command: irc.ERR_USERNOTINCHANNEL,
					Params:  []string{msg.Prefix.Name, nick, channelname, "They aren't on that channel"},
				})
			} else {
				// If the user already is a chanop, silently do
				// nothing (like UnrealIRCd).
				if perms[chanop] != newvalue {
					c.nicks[NickToLower(nick)][chanop] = newvalue
				}
			}
		default:
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_UNKNOWNMODE,
				Params:  []string{msg.Prefix.Name, string(char), "is unknown mode char to me"},
			})
		}
	}
	if reply.replyid > 0 {
		return
	}
	i.sendChannel(c, reply, &irc.Message{
		Prefix:  servicesPrefix(msg.Prefix),
		Command: irc.MODE,
		Params:  append([]string{channelname}, modeCmds(modes).IRCParams()...),
	})
}

func (i *IRCServer) cmdServer(s *Session, reply *Replyctx, msg *irc.Message) {
	authenticated := false
	for _, service := range i.Config.IRC.Services {
		if s.Pass == "services="+service.Password {
			authenticated = true
			break
		}
	}
	if !authenticated {
		i.sendUser(s, reply, &irc.Message{
			Command: irc.ERROR,
			Params:  []string{"Invalid password"},
		})
		return
	}
	s.Server = true
	s.ircPrefix = irc.Prefix{
		Name: msg.Params[0],
	}
	i.serverSessions = append(i.serverSessions, s.Id.Id)
	i.sendServices(reply, &irc.Message{
		Command: "SERVER",
		Params: []string{
			i.ServerPrefix.Name,
			"1",  // hopcount
			"23", // token, must be different from the services token
		},
	})
	nicks := make([]string, 0, len(i.nicks))
	for nick := range i.nicks {
		nicks = append(nicks, string(nick))
	}
	sort.Strings(nicks)
	for _, nick := range nicks {
		session := i.nicks[lcNick(nick)]
		// Skip sessions that are not yet logged in, sessions that represent a
		// server connection and subsessions of a server connection.
		if !session.loggedIn || session.Server || session.Id.Reply != 0 {
			continue
		}
		modestr := "+"
		for mode := 'A'; mode < 'z'; mode++ {
			if session.modes[mode] {
				modestr += string(mode)
			}
		}
		i.sendServices(reply, &irc.Message{
			Command: irc.NICK,
			Params: []string{
				session.Nick,
				"1", // hopcount (ignored by anope)
				"1", // timestamp
				session.Username,
				session.ircPrefix.Host,
				i.ServerPrefix.Name,
				session.svid,
				modestr,
				session.Realname,
			},
		})
		channelnames := make([]string, 0, len(session.Channels))
		for channelname := range session.Channels {
			channelnames = append(channelnames, string(channelname))
		}
		sort.Strings(channelnames)
		for _, channelname := range channelnames {
			var prefix string

			if i.channels[lcChan(channelname)].nicks[NickToLower(session.Nick)][chanop] {
				prefix = prefix + string('@')
			}
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: "SJOIN",
				Params:  []string{"1", i.channels[lcChan(channelname)].name, prefix + session.Nick},
			})
		}
	}
}

func (i *IRCServer) cmdServerSvshold(s *Session, reply *Replyctx, msg *irc.Message) {
	// SVSHOLD <nick> [<expirationtimerelative> :<reason>]
	nick := NickToLower(msg.Params[0])
	if len(msg.Params) > 1 {
		duration, err := time.ParseDuration(msg.Params[1] + "s")
		if err != nil {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.NOTICE,
				Params:  []string{s.ircPrefix.Name, fmt.Sprintf("Invalid duration: %v", err)},
			})
			return
		}
		i.svsholds[nick] = svshold{
			added:    s.LastActivity,
			duration: duration,
			reason:   msg.Trailing(),
		}
	} else {
		delete(i.svsholds, nick)
	}
}

func (i *IRCServer) cmdServerSvsjoin(s *Session, reply *Replyctx, msg *irc.Message) {
	// SVSJOIN <nick> <chan>
	nick := NickToLower(msg.Params[0])
	channelname := msg.Params[1]

	session, ok := i.nicks[nick]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{msg.Prefix.Name, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	if !IsValidChannel(channelname) {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, channelname, "No such channel"},
		})
		return
	}
	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		c = &channel{
			name:  channelname,
			nicks: make(map[lcNick]*[maxChanMemberStatus]bool),
		}
		i.channels[ChanToLower(channelname)] = c
	}
	if _, ok := c.nicks[nick]; ok {
		return
	}
	c.nicks[nick] = &[maxChanMemberStatus]bool{}
	// If the channel did not exist before, the first joining user becomes a
	// channel operator.
	if !ok {
		c.nicks[nick][chanop] = true
	}
	session.Channels[ChanToLower(channelname)] = true

	i.sendChannel(c, reply, &irc.Message{
		Prefix:  &session.ircPrefix,
		Command: irc.JOIN,
		Params:  []string{channelname},
	})
	var prefix string
	if c.nicks[nick][chanop] {
		prefix = prefix + string('@')
	}
	i.sendServices(reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: "SJOIN",
		Params:  []string{"1", channelname, prefix + session.Nick},
	})
	// Integrate the topic response by simulating a TOPIC command.
	i.cmdTopic(session, reply, &irc.Message{Command: irc.TOPIC, Params: []string{channelname}})
	i.cmdNames(session, reply, &irc.Message{Command: irc.NAMES, Params: []string{channelname}})
}

func (i *IRCServer) cmdServerSvspart(s *Session, reply *Replyctx, msg *irc.Message) {
	// SVSPART <nick> <chan>
	nick := NickToLower(msg.Params[0])
	channelname := msg.Params[1]

	session, ok := i.nicks[nick]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{msg.Prefix.Name, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, channelname, "No such channel"},
		})
		return
	}

	if _, ok := c.nicks[nick]; !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTONCHANNEL,
			Params:  []string{msg.Prefix.Name, channelname, "You're not on that channel"},
		})
		return
	}

	i.sendServices(reply, i.sendChannel(c, reply, &irc.Message{
		Prefix:  &session.ircPrefix,
		Command: irc.PART,
		Params:  []string{channelname},
	}))

	delete(c.nicks, nick)
	i.maybeDeleteChannel(c)
	delete(session.Channels, ChanToLower(channelname))
}

func (i *IRCServer) cmdServerSvsmode(s *Session, reply *Replyctx, msg *irc.Message) {
	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{"*", msg.Params[0], "No such nick/channel"},
		})
		return
	}
	modestr := msg.Params[1]
	if !strings.HasPrefix(modestr, "+") && !strings.HasPrefix(modestr, "-") {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_UMODEUNKNOWNFLAG,
			Params:  []string{"*", "Unknown MODE flag"},
		})
		return
	}
	modes := normalizeModes(msg)

	// true for adding a mode, false for removing it
	for _, mode := range modes {
		newvalue := (mode.Mode[0] == '+')
		char := mode.Mode[1]
		switch char {
		case 'd':
			session.svid = mode.Param
		case 'r':
			// Store registered flag
			session.modes[char] = newvalue
		default:
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_UMODEUNKNOWNFLAG,
				Params:  []string{"*", "Unknown MODE flag"},
			})
		}
	}
	modestr = "+"
	for mode := 'A'; mode < 'z'; mode++ {
		if session.modes[mode] {
			modestr += string(mode)
		}
	}
	i.sendUser(session, reply, &irc.Message{
		Prefix:  &s.ircPrefix,
		Command: irc.MODE,
		Params:  []string{session.Nick, modestr},
	})
}

func (i *IRCServer) cmdServerSvsnick(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “SVSNICK blArgh Guest30503 :1425036445”
	if !IsValidNickname(msg.Params[1]) {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_ERRONEUSNICKNAME,
			Params:  []string{"*", msg.Params[1], "Erroneous nickname"},
		})
		return
	}

	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{"*", msg.Params[0], "No such nick/channel"},
		})
		return
	}

	// TODO(secure): kill this code duplication with cmdNick()
	oldPrefix := session.ircPrefix
	oldNick := NickToLower(msg.Params[0])
	session.Nick = msg.Params[1]
	i.nicks[NickToLower(session.Nick)] = session
	delete(i.nicks, oldNick)
	for _, c := range i.channels {
		if modes, ok := c.nicks[oldNick]; ok {
			c.nicks[NickToLower(session.Nick)] = modes
		}
		delete(c.nicks, oldNick)
	}
	session.updateIrcPrefix()
	i.sendServices(reply,
		i.sendCommonChannels(session, reply,
			i.sendUser(session, reply, &irc.Message{
				Prefix:  &oldPrefix,
				Command: irc.NICK,
				Params:  []string{session.Nick},
			})))
}

func (i *IRCServer) cmdServerJoin(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “:ChanServ JOIN #noname-ev” (before enforcing AKICK).

	for _, channelname := range strings.Split(msg.Params[0], ",") {
		if !IsValidChannel(channelname) {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{msg.Prefix.Name, channelname, "No such channel"},
			})
			continue
		}

		nick := NickToLower(msg.Prefix.Name)
		session, ok := i.nicks[nick]
		if !ok {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHNICK,
				Params:  []string{msg.Prefix.Name, channelname, "No such nick/channel"},
			})
			continue
		}

		// TODO(secure): reduce code duplication with cmdJoin()
		c, ok := i.channels[ChanToLower(channelname)]
		if !ok {
			c = &channel{
				name:  channelname,
				nicks: make(map[lcNick]*[maxChanMemberStatus]bool),
			}
			i.channels[ChanToLower(channelname)] = c
		}
		c.nicks[nick] = &[maxChanMemberStatus]bool{}
		// If the channel did not exist before, the first joining user becomes a
		// channel operator.
		if !ok {
			c.nicks[nick][chanop] = true
		}
		session.Channels[ChanToLower(channelname)] = true

		i.sendCommonChannels(session, reply, &irc.Message{
			Prefix:  servicesPrefix(msg.Prefix),
			Command: irc.JOIN,
			Params:  []string{channelname},
		})
	}
}

func (i *IRCServer) cmdServerPart(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “:ChanServ PART #noname-ev” (after enforcing AKICK).
	for _, channelname := range strings.Split(msg.Params[0], ",") {
		c, ok := i.channels[ChanToLower(channelname)]
		if !ok {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{msg.Prefix.Name, channelname, "No such channel"},
			})
			continue
		}

		if _, ok := c.nicks[NickToLower(msg.Prefix.Name)]; !ok {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOTONCHANNEL,
				Params:  []string{msg.Prefix.Name, channelname, "You're not on that channel"},
			})
			continue
		}
		session, _ := i.nicks[NickToLower(msg.Prefix.Name)]

		i.sendCommonChannels(session, reply, &irc.Message{
			Prefix:  servicesPrefix(msg.Prefix),
			Command: irc.PART,
			Params:  []string{channelname},
		})

		// TODO(secure): reduce code duplication with cmdPart()
		delete(c.nicks, NickToLower(msg.Prefix.Name))
		i.maybeDeleteChannel(c)
		delete(session.Channels, ChanToLower(channelname))
	}
}

func (i *IRCServer) cmdServerTopic(s *Session, reply *Replyctx, msg *irc.Message) {
	// e.g. “:ChanServ TOPIC #chaos-hd ChanServ 0 :”
	channel := msg.Params[0]
	c, ok := i.channels[ChanToLower(channel)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, channel, "No such channel"},
		})
		return
	}

	// “TOPIC :”, i.e. unset the topic.
	if msg.Trailing() == "" && len(msg.Params) == 2 {
		c.topicNick = ""
		c.topicTime = time.Time{}
		c.topic = ""
		i.sendChannel(c, reply, &irc.Message{
			Prefix:  servicesPrefix(msg.Prefix),
			Command: irc.TOPIC,
			Params:  []string{channel, msg.Trailing()},
		})
		return
	}

	ts, err := strconv.ParseInt(msg.Params[2], 0, 64)
	if err != nil {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NEEDMOREPARAMS,
			Params:  []string{"*", channel, fmt.Sprintf("Could not parse timestamp: %v", err)},
		})
		return
	}

	c.topicNick = msg.Params[1]
	c.topicTime = time.Unix(ts, 0)
	c.topic = msg.Trailing()

	i.sendChannel(c, reply, &irc.Message{
		Prefix:  servicesPrefix(msg.Prefix),
		Command: irc.TOPIC,
		Params:  []string{channel, msg.Trailing()},
	})
}

// The only difference is that we re-use (and augment) the msg.Prefix instead of setting s.Prefix.
// TODO(secure): refactor this with cmdPrivmsg possibly?
func (i *IRCServer) cmdServerPrivmsg(s *Session, reply *Replyctx, msg *irc.Message) {
	if len(msg.Params) < 1 {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NORECIPIENT,
			Params:  []string{msg.Prefix.Name, fmt.Sprintf("No recipient given (%s)", msg.Command)},
		})
		return
	}

	if msg.Trailing() == "" {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOTEXTTOSEND,
			Params:  []string{msg.Prefix.Name, "No text to send"},
		})
		return
	}

	if strings.HasPrefix(msg.Params[0], "#") {
		c, ok := i.channels[ChanToLower(msg.Params[0])]
		if !ok {
			i.sendServices(reply, &irc.Message{
				Prefix:  i.ServerPrefix,
				Command: irc.ERR_NOSUCHCHANNEL,
				Params:  []string{msg.Prefix.Name, msg.Params[0], "No such channel"},
			})
			return
		}
		i.sendChannel(c, reply, &irc.Message{
			Prefix:  servicesPrefix(msg.Prefix),
			Command: msg.Command,
			Params:  []string{msg.Params[0], msg.Trailing()},
		})
		return
	}

	session, ok := i.nicks[NickToLower(msg.Params[0])]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{msg.Prefix.Name, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	i.sendUser(session, reply, &irc.Message{
		Prefix:  servicesPrefix(msg.Prefix),
		Command: msg.Command,
		Params:  []string{msg.Params[0], msg.Trailing()},
	})
}

// TODO(secure): refactor this with cmdInvite possibly?
func (i *IRCServer) cmdServerInvite(s *Session, reply *Replyctx, msg *irc.Message) {
	nickname := msg.Params[0]
	channelname := msg.Params[1]

	session, ok := i.nicks[NickToLower(nickname)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHNICK,
			Params:  []string{msg.Prefix.Name, msg.Params[0], "No such nick/channel"},
		})
		return
	}

	c, ok := i.channels[ChanToLower(channelname)]
	if !ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_NOSUCHCHANNEL,
			Params:  []string{msg.Prefix.Name, msg.Params[1], "No such channel"},
		})
		return
	}

	if _, ok := c.nicks[NickToLower(nickname)]; ok {
		i.sendServices(reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_USERONCHANNEL,
			Params:  []string{msg.Prefix.Name, session.Nick, c.name, "is already on channel"},
		})
		return
	}

	session.invitedTo[ChanToLower(channelname)] = true
	i.sendServices(reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_INVITING,
		Params:  []string{msg.Prefix.Name, msg.Params[0], c.name},
	})
	i.sendUser(session, reply, &irc.Message{
		Prefix:  servicesPrefix(msg.Prefix),
		Command: irc.INVITE,
		Params:  []string{session.Nick, c.name},
	})
	i.sendChannel(c, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.NOTICE,
		Params:  []string{c.name, fmt.Sprintf("%s invited %s into the channel.", msg.Prefix.Name, msg.Params[0])},
	})
}
