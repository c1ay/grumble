// Copyright (c) 2010-2011 The Grumble Authors
// The use of this source code is goverened by a BSD-style
// license that can be found in the LICENSE-file.

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"goprotobuf.googlecode.com/hg/proto"
	"grumble/ban"
	"grumble/blobstore"
	"grumble/cryptstate"
	"grumble/freezer"
	"grumble/htmlfilter"
	"grumble/logtarget"
	"grumble/mumbleproto"
	"grumble/serverconf"
	"grumble/sessionpool"
	"hash"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The default port a Murmur server listens on
const DefaultPort = 64738
const UDPPacketSize = 1024

const LogOpsBeforeSync = 100
const CeltCompatBitstream = -2147483637
const (
	StateClientConnected = iota
	StateServerSentVersion
	StateClientSentVersion
	StateClientAuthenticated
	StateClientReady
	StateClientDead
)

type KeyValuePair struct {
	Key   string
	Value string
}

// A Murmur server instance
type Server struct {
	Id       int64
	listener tls.Listener
	address  string
	port     int
	udpconn  *net.UDPConn
	tlscfg   *tls.Config
	running  bool

	incoming       chan *Message
	udpsend        chan *Message
	voicebroadcast chan *VoiceBroadcast
	cfgUpdate      chan *KeyValuePair

	// Signals to the server that a client has been successfully
	// authenticated.
	clientAuthenticated chan *Client

	// Server configuration
	cfg *serverconf.Config

	// Clients
	clients map[uint32]*Client

	// Host, host/port -> client mapping
	hmutex    sync.Mutex
	hclients  map[string][]*Client
	hpclients map[string]*Client

	// Codec information
	AlphaCodec       int32
	BetaCodec        int32
	PreferAlphaCodec bool

	// Channels
	Channels   map[int]*Channel
	nextChanId int

	// Users
	Users       map[uint32]*User
	UserCertMap map[string]*User
	UserNameMap map[string]*User
	nextUserId  uint32

	// Sessions
	pool *sessionpool.SessionPool

	// Freezer
	numLogOps int
	freezelog *freezer.Log

	// ACL cache
	aclcache ACLCache

	// Bans
	banlock sync.RWMutex
	Bans    []ban.Ban

	// Logging
	*log.Logger
}

type clientLogForwarder struct {
	client *Client
	logger *log.Logger
}

func (lf clientLogForwarder) Write(incoming []byte) (int, error) {
	buf := new(bytes.Buffer)
	buf.WriteString(fmt.Sprintf("<%v:%v(%v)> ", lf.client.Session, lf.client.ShownName(), lf.client.UserId()))
	buf.Write(incoming)
	lf.logger.Output(3, buf.String())
	return len(incoming), nil
}

// Allocate a new Murmur instance
func NewServer(id int64, addr string, port int) (s *Server, err error) {
	s = new(Server)

	s.Id = id
	s.address = addr
	s.port = port
	s.running = false

	s.cfg = serverconf.New(nil)

	s.pool = sessionpool.New()
	s.clients = make(map[uint32]*Client)
	s.Users = make(map[uint32]*User)
	s.UserCertMap = make(map[string]*User)
	s.UserNameMap = make(map[string]*User)

	s.hclients = make(map[string][]*Client)
	s.hpclients = make(map[string]*Client)

	s.incoming = make(chan *Message)
	s.udpsend = make(chan *Message)
	s.voicebroadcast = make(chan *VoiceBroadcast)
	s.cfgUpdate = make(chan *KeyValuePair)
	s.clientAuthenticated = make(chan *Client)

	s.Users[0], err = NewUser(0, "SuperUser")
	s.UserNameMap["SuperUser"] = s.Users[0]
	s.nextUserId = 1

	s.Channels = make(map[int]*Channel)
	s.aclcache = NewACLCache()

	// Create root channel
	s.Channels[0] = NewChannel(0, "Root")
	s.nextChanId = 1

	s.Logger = log.New(&logtarget.Target, fmt.Sprintf("[%v] ", s.Id), log.LstdFlags|log.Lmicroseconds)

	return
}

// Get a pointer to the root channel
func (server *Server) RootChannel() *Channel {
	root, exists := server.Channels[0]
	if !exists {
		server.Fatalf("Not Root channel found for server")
	}
	return root
}

// Set password as the new SuperUser password
func (server *Server) SetSuperUserPassword(password string) {
	saltBytes := make([]byte, 24)
	_, err := rand.Read(saltBytes)
	if err != nil {
		server.Fatalf("Unable to read from crypto/rand: %v", err)
	}

	salt := hex.EncodeToString(saltBytes)
	hasher := sha1.New()
	hasher.Write(saltBytes)
	hasher.Write([]byte(password))
	digest := hex.EncodeToString(hasher.Sum())

	// Could be racy, but shouldn't really matter...
	key := "SuperUserPassword"
	val := "sha1$" + salt + "$" + digest
	server.cfg.Set(key, val)
	server.cfgUpdate <- &KeyValuePair{Key: key, Value: val}
}

// Check whether password matches the set SuperUser password.
func (server *Server) CheckSuperUserPassword(password string) bool {
	parts := strings.Split(server.cfg.StringValue("SuperUserPassword"), "$")
	if len(parts) != 3 {
		return false
	}

	if len(parts[2]) == 0 {
		return false
	}

	var h hash.Hash
	switch parts[0] {
	case "sha1":
		h = sha1.New()
	default:
		// no such hash
		return false
	}

	// salt
	if len(parts[1]) > 0 {
		saltBytes, err := hex.DecodeString(parts[1])
		if err != nil {
			server.Fatalf("Unable to decode salt: %v", err)
		}
		h.Write(saltBytes)
	}

	// password
	h.Write([]byte(password))

	sum := hex.EncodeToString(h.Sum())
	if parts[2] == sum {
		return true
	}

	return false
}

// Called by the server to initiate a new client connection.
func (server *Server) NewClient(conn net.Conn) (err error) {
	client := new(Client)
	addr := conn.RemoteAddr()
	if addr == nil {
		err = errors.New("Unable to extract address for client.")
		return
	}

	client.lf = &clientLogForwarder{client, server.Logger}
	client.Logger = log.New(client.lf, "", 0)

	client.Session = server.pool.Get()
	client.Printf("New connection: %v (%v)", conn.RemoteAddr(), client.Session)

	client.tcpaddr = addr.(*net.TCPAddr)
	client.server = server
	client.conn = conn
	client.reader = bufio.NewReader(client.conn)
	client.writer = bufio.NewWriter(client.conn)
	client.state = StateClientConnected

	client.msgchan = make(chan *Message)
	client.udprecv = make(chan []byte)

	client.user = nil

	go client.receiver()
	go client.udpreceiver()

	client.doneSending = make(chan bool)
	go client.sender()

	return
}

// Remove a disconnected client from the server's
// internal representation.
func (server *Server) RemoveClient(client *Client, kicked bool) {
	server.hmutex.Lock()
	host := client.tcpaddr.IP.String()
	oldclients := server.hclients[host]
	newclients := []*Client{}
	for _, hostclient := range oldclients {
		if hostclient != client {
			newclients = append(newclients, hostclient)
		}
	}
	server.hclients[host] = newclients
	if client.udpaddr != nil {
		delete(server.hpclients, client.udpaddr.String())
	}
	server.hmutex.Unlock()

	delete(server.clients, client.Session)
	server.pool.Reclaim(client.Session)

	// Remove client from channel
	channel := client.Channel
	if channel != nil {
		channel.RemoveClient(client)
	}

	// If the user was not kicked, broadcast a UserRemove message.
	// If the user is disconnect via a kick, the UserRemove message has already been sent
	// at this point.
	if !kicked && client.state > StateClientAuthenticated {
		err := server.broadcastProtoMessage(&mumbleproto.UserRemove{
			Session: proto.Uint32(client.Session),
		})
		if err != nil {
			server.Panic("Unable to broadcast UserRemove message for disconnected client.")
		}
	}
}

// Add a new channel to the server. Automatically assign it a channel ID.
func (server *Server) AddChannel(name string) (channel *Channel) {
	channel = NewChannel(server.nextChanId, name)
	server.Channels[channel.Id] = channel
	server.nextChanId += 1

	return
}

// Remove a channel from the server.
func (server *Server) RemoveChanel(channel *Channel) {
	if channel.Id == 0 {
		server.Printf("Attempted to remove root channel.")
		return
	}

	delete(server.Channels, channel.Id)
}

// Link two channels
func (server *Server) LinkChannels(channel *Channel, other *Channel) {
	channel.Links[other.Id] = other
	other.Links[channel.Id] = channel
}

// Unlink two channels
func (server *Server) UnlinkChannels(channel *Channel, other *Channel) {
	delete(channel.Links, other.Id)
	delete(other.Links, channel.Id)
}

// This is the synchronous handler goroutine.
// Important control channel messages are routed through this Goroutine
// to keep server state synchronized.
func (server *Server) handler() {
	regtick := time.Tick((3600 + ((server.Id * 60) % 600)) * 1e9)
	for {
		select {
		// Control channel messages
		case msg := <-server.incoming:
			client := msg.client
			server.handleIncomingMessage(client, msg)
		// Voice broadcast
		case vb := <-server.voicebroadcast:
			server.Printf("VoiceBroadcast!")
			if vb.target == 0 {
				channel := vb.client.Channel
				for _, client := range channel.clients {
					if client != vb.client {
						client.sendUdp(&Message{
							buf:    vb.buf,
							client: client,
						})
					}
				}
			}
		// Finish client authentication. Send post-authentication
		// server info.
		case client := <-server.clientAuthenticated:
			server.finishAuthenticate(client)

		// Disk freeze config update
		case kvp := <-server.cfgUpdate:
			server.UpdateConfig(kvp.Key, kvp.Value)

		// Server registration update
		// Tick every hour + a minute offset based on the server id.
		case <-regtick:
			server.RegisterPublicServer()
		}

		// Check if its time to sync the server state and re-open the log
		if server.numLogOps >= LogOpsBeforeSync {
			server.Print("Writing full server snapshot to disk")
			err := server.FreezeToFile()
			if err != nil {
				server.Fatal(err)
			}
			server.numLogOps = 0
			server.Print("Wrote full server snapshot to disk")
		}
	}
}

// Handle an Authenticate protobuf message.  This is handled in a separate
// goroutine to allow for remote authenticators that are slow to respond.
//
// Once a user has been authenticated, it will ping the server's handler
// routine, which will call the finishAuthenticate method on Server which
// will send the channel tree, user list, etc. to the client.
func (server *Server) handleAuthenticate(client *Client, msg *Message) {
	// Is this message not an authenticate message? If not, discard it...
	if msg.kind != mumbleproto.MessageAuthenticate {
		client.Panic("Unexpected message. Expected Authenticate.")
		return
	}

	auth := &mumbleproto.Authenticate{}
	err := proto.Unmarshal(msg.buf, auth)
	if err != nil {
		client.Panic("Unable to unmarshal Authenticate message.")
		return
	}

	// Set access tokens. Clients can set their access tokens any time
	// by sending an Authenticate message with he contents of their new
	// access token list.
	client.Tokens = auth.Tokens
	server.ClearACLCache()

	if client.state >= StateClientAuthenticated {
		return
	}

	// Did we get a username?
	if auth.Username == nil || len(*auth.Username) == 0 {
		client.RejectAuth(mumbleproto.Reject_InvalidUsername, "Please specify a username to log in")
		return
	}

	client.Username = *auth.Username

	// Extract certhash
	tlsconn, ok := client.conn.(*tls.Conn)
	if !ok {
		client.Panic("Invalid connection")
		return
	}
	state := tlsconn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		hash := sha1.New()
		hash.Write(state.PeerCertificates[0].Raw)
		sum := hash.Sum()
		client.CertHash = hex.EncodeToString(sum)
	}

	if client.Username == "SuperUser" {
		if auth.Password == nil {
			client.RejectAuth(mumbleproto.Reject_WrongUserPW, "")
			return
		} else {
			if server.CheckSuperUserPassword(*auth.Password) {
				client.user, ok = server.UserNameMap[client.Username]
				if !ok {
					client.RejectAuth(mumbleproto.Reject_InvalidUsername, "")
					return
				}
			} else {
				client.RejectAuth(mumbleproto.Reject_WrongUserPW, "")
				return
			}
		}
	} else {
		// First look up registration by name.
		user, exists := server.UserNameMap[client.Username]
		if exists {
			if len(client.CertHash) > 0 && user.CertHash == client.CertHash {
				client.user = user
			} else {
				client.RejectAuth(mumbleproto.Reject_WrongUserPW, "Wrong certificate hash")
				return
			}
		}

		// Name matching didn't do.  Try matching by certificate.
		if client.user == nil && len(client.CertHash) > 0 {
			user, exists := server.UserCertMap[client.CertHash]
			if exists {
				client.user = user
			}
		}
	}

	// Setup the cryptstate for the client.
	client.crypt, err = cryptstate.New()
	if err != nil {
		client.Panicf("%v", err)
		return
	}
	err = client.crypt.GenerateKey()
	if err != nil {
		client.Panicf("%v", err)
		return
	}

	// Send CryptState information to the client so it can establish an UDP connection,
	// if it wishes.
	client.lastResync = time.Seconds()
	err = client.sendProtoMessage(&mumbleproto.CryptSetup{
		Key:         client.crypt.RawKey[0:],
		ClientNonce: client.crypt.DecryptIV[0:],
		ServerNonce: client.crypt.EncryptIV[0:],
	})
	if err != nil {
		client.Panicf("%v", err)
	}

	// Add codecs
	client.codecs = auth.CeltVersions
	if len(client.codecs) == 0 {
		server.Printf("Client %i connected without CELT codecs.", client.Session)
	}

	client.state = StateClientAuthenticated
	server.clientAuthenticated <- client
}

// The last part of authentication runs in the server's synchronous handler.
func (server *Server) finishAuthenticate(client *Client) {
	// If the client succeeded in proving to the server that it should be granted
	// the credentials of a registered user, do some sanity checking to make sure
	// that user isn't already connected.
	//
	// If the user is already connected, try to check whether this new client is
	// connecting from the same IP address. If that's the case, disconnect the
	// previous client and let the new guy in.
	if client.user != nil {
		found := false
		for _, connectedClient := range server.clients {
			if connectedClient.UserId() == client.UserId() {
				found = true
				break
			}
		}
		// The user is already present on the server.
		if found {
			// todo(mkrautz): Do the address checking.
			client.RejectAuth(mumbleproto.Reject_UsernameInUse, "A client is already connected using those credentials.")
			return
		}

		// No, that user isn't already connected. Move along.
	}

	// Add the client to the connected list
	server.clients[client.Session] = client

	// First, check whether we need to tell the other connected
	// clients to switch to a codec so the new guy can actually speak.
	server.updateCodecVersions()

	client.sendChannelList()

	// Add the client to the host slice for its host address.
	host := client.tcpaddr.IP.String()
	server.hmutex.Lock()
	server.hclients[host] = append(server.hclients[host], client)
	server.hmutex.Unlock()

	userstate := &mumbleproto.UserState{
		Session:   proto.Uint32(client.Session),
		Name:      proto.String(client.ShownName()),
		ChannelId: proto.Uint32(0),
	}

	if len(client.CertHash) > 0 {
		userstate.Hash = proto.String(client.CertHash)
	}

	if client.IsRegistered() {
		userstate.UserId = proto.Uint32(uint32(client.UserId()))

		if client.user.HasTexture() {
			// Does the client support blobs?
			if client.Version >= 0x10203 {
				userstate.TextureHash = client.user.TextureBlobHashBytes()
			} else {
				buf, err := blobstore.Get(client.user.TextureBlob)
				if err != nil {
					server.Panicf("Blobstore error: %v", err.Error())
				}
				userstate.Texture = buf
			}
		}

		if client.user.HasComment() {
			// Does the client support blobs?
			if client.Version >= 0x10203 {
				userstate.CommentHash = client.user.CommentBlobHashBytes()
			} else {
				buf, err := blobstore.Get(client.user.CommentBlob)
				if err != nil {
					server.Panicf("Blobstore error: %v", err.Error())
				}
				userstate.Comment = proto.String(string(buf))
			}
		}
	}

	server.userEnterChannel(client, server.RootChannel(), userstate)
	if err := server.broadcastProtoMessage(userstate); err != nil {
		// Server panic?
	}

	server.sendUserList(client)

	sync := &mumbleproto.ServerSync{}
	sync.Session = proto.Uint32(client.Session)
	sync.MaxBandwidth = proto.Uint32(server.cfg.Uint32Value("MaxBandwidth"))
	sync.WelcomeText = proto.String(server.cfg.StringValue("WelcomeText"))
	if client.IsSuperUser() {
		sync.Permissions = proto.Uint64(uint64(AllPermissions))
	} else {
		server.HasPermission(client, server.RootChannel(), EnterPermission)
		perm := server.aclcache.GetPermission(client, server.RootChannel())
		if !perm.IsCached() {
			client.Panic("Corrupt ACL cache")
			return
		}
		perm.ClearCacheBit()
		sync.Permissions = proto.Uint64(uint64(perm))
	}
	if err := client.sendProtoMessage(sync); err != nil {
		client.Panicf("%v", err)
		return
	}

	err := client.sendProtoMessage(&mumbleproto.ServerConfig{
		AllowHtml:          proto.Bool(server.cfg.BoolValue("AllowHTML")),
		MessageLength:      proto.Uint32(server.cfg.Uint32Value("MaxTextMessageLength")),
		ImageMessageLength: proto.Uint32(server.cfg.Uint32Value("MaxImageMessageLength")),
	})
	if err != nil {
		client.Panicf("%v", err)
		return
	}

	client.state = StateClientReady
	client.clientReady <- true
}

func (server *Server) updateCodecVersions() {
	codecusers := map[int32]int{}
	var winner int32
	var count int

	for _, client := range server.clients {
		for _, codec := range client.codecs {
			codecusers[codec] += 1
		}
	}

	for codec, users := range codecusers {
		if users > count {
			count = users
			winner = codec
		}
		if users == count && codec > winner {
			winner = codec
		}
	}

	var current int32
	if server.PreferAlphaCodec {
		current = server.AlphaCodec
	} else {
		current = server.BetaCodec
	}

	if winner == current {
		return
	}

	if winner == CeltCompatBitstream {
		server.PreferAlphaCodec = true
	} else {
		server.PreferAlphaCodec = !server.PreferAlphaCodec
	}

	if server.PreferAlphaCodec {
		server.AlphaCodec = winner
	} else {
		server.BetaCodec = winner
	}

	err := server.broadcastProtoMessage(&mumbleproto.CodecVersion{
		Alpha:       proto.Int32(server.AlphaCodec),
		Beta:        proto.Int32(server.BetaCodec),
		PreferAlpha: proto.Bool(server.PreferAlphaCodec),
	})
	if err != nil {
		server.Printf("Unable to broadcast.")
		return
	}

	server.Printf("CELT codec switch %#x %#x (PreferAlpha %v)", uint32(server.AlphaCodec), uint32(server.BetaCodec), server.PreferAlphaCodec)
	return
}

func (server *Server) sendUserList(client *Client) {
	for _, connectedClient := range server.clients {
		if connectedClient.state != StateClientReady {
			continue
		}
		if connectedClient == client {
			continue
		}

		userstate := &mumbleproto.UserState{
			Session:   proto.Uint32(connectedClient.Session),
			Name:      proto.String(connectedClient.ShownName()),
			ChannelId: proto.Uint32(uint32(connectedClient.Channel.Id)),
		}

		if len(connectedClient.CertHash) > 0 {
			userstate.Hash = proto.String(connectedClient.CertHash)
		}

		if connectedClient.IsRegistered() {
			userstate.UserId = proto.Uint32(uint32(connectedClient.UserId()))

			if connectedClient.user.HasTexture() {
				// Does the client support blobs?
				if client.Version >= 0x10203 {
					userstate.TextureHash = connectedClient.user.TextureBlobHashBytes()
				} else {
					buf, err := blobstore.Get(connectedClient.user.TextureBlob)
					if err != nil {
						server.Panicf("Blobstore error: %v", err.Error())
					}
					userstate.Texture = buf
				}
			}

			if connectedClient.user.HasComment() {
				// Does the client support blobs?
				if client.Version >= 0x10203 {
					userstate.CommentHash = connectedClient.user.CommentBlobHashBytes()
				} else {
					buf, err := blobstore.Get(connectedClient.user.CommentBlob)
					if err != nil {
						server.Panicf("Blobstore error: %v", err.Error())
					}
					userstate.Comment = proto.String(string(buf))
				}
			}
		}

		if connectedClient.Mute {
			userstate.Mute = proto.Bool(true)
		}
		if connectedClient.Suppress {
			userstate.Suppress = proto.Bool(true)
		}
		if connectedClient.SelfMute {
			userstate.SelfMute = proto.Bool(true)
		}
		if connectedClient.SelfDeaf {
			userstate.SelfDeaf = proto.Bool(true)
		}
		if connectedClient.PrioritySpeaker {
			userstate.PrioritySpeaker = proto.Bool(true)
		}
		if connectedClient.Recording {
			userstate.Recording = proto.Bool(true)
		}
		if connectedClient.PluginContext != nil || len(connectedClient.PluginContext) > 0 {
			userstate.PluginContext = connectedClient.PluginContext
		}
		if len(connectedClient.PluginIdentity) > 0 {
			userstate.PluginIdentity = proto.String(connectedClient.PluginIdentity)
		}

		err := client.sendProtoMessage(userstate)
		if err != nil {
			// Server panic?
			continue
		}
	}
}

// Send a client its permissions for channel.
func (server *Server) sendClientPermissions(client *Client, channel *Channel) {
	// No caching for SuperUser
	if client.IsSuperUser() {
		return
	}

	// Update cache
	server.HasPermission(client, channel, EnterPermission)
	perm := server.aclcache.GetPermission(client, channel)

	// fixme(mkrautz): Cache which permissions we've already sent.
	client.sendProtoMessage(&mumbleproto.PermissionQuery{
		ChannelId:   proto.Uint32(uint32(channel.Id)),
		Permissions: proto.Uint32(uint32(perm)),
	})
}

type ClientPredicate func(client *Client) bool

func (server *Server) broadcastProtoMessageWithPredicate(msg interface{}, clientcheck ClientPredicate) error {
	for _, client := range server.clients {
		if !clientcheck(client) {
			continue
		}
		if client.state < StateClientAuthenticated {
			continue
		}
		err := client.sendProtoMessage(msg)
		if err != nil {
			return err
		}
	}

	return nil
}

func (server *Server) broadcastProtoMessage(msg interface{}) (err error) {
	err = server.broadcastProtoMessageWithPredicate(msg, func(client *Client) bool { return true })
	return
}

func (server *Server) handleIncomingMessage(client *Client, msg *Message) {
	switch msg.kind {
	case mumbleproto.MessageAuthenticate:
		server.handleAuthenticate(msg.client, msg)
	case mumbleproto.MessagePing:
		server.handlePingMessage(msg.client, msg)
	case mumbleproto.MessageChannelRemove:
		server.handleChannelRemoveMessage(msg.client, msg)
	case mumbleproto.MessageChannelState:
		server.handleChannelStateMessage(msg.client, msg)
	case mumbleproto.MessageUserState:
		server.handleUserStateMessage(msg.client, msg)
	case mumbleproto.MessageUserRemove:
		server.handleUserRemoveMessage(msg.client, msg)
	case mumbleproto.MessageBanList:
		server.handleBanListMessage(msg.client, msg)
	case mumbleproto.MessageTextMessage:
		server.handleTextMessage(msg.client, msg)
	case mumbleproto.MessageACL:
		server.handleAclMessage(msg.client, msg)
	case mumbleproto.MessageQueryUsers:
		server.handleQueryUsers(msg.client, msg)
	case mumbleproto.MessageCryptSetup:
		server.handleCryptSetup(msg.client, msg)
	case mumbleproto.MessageContextAction:
		server.Printf("MessageContextAction from client")
	case mumbleproto.MessageUserList:
		server.handleUserList(msg.client, msg)
	case mumbleproto.MessageVoiceTarget:
		server.Printf("MessageVoiceTarget from client")
	case mumbleproto.MessagePermissionQuery:
		server.handlePermissionQuery(msg.client, msg)
	case mumbleproto.MessageUserStats:
		server.handleUserStatsMessage(msg.client, msg)
	case mumbleproto.MessageRequestBlob:
		server.handleRequestBlob(msg.client, msg)
	}
}

func (s *Server) SetupUDP() (err error) {
	addr := &net.UDPAddr{
		net.ParseIP(s.address),
		s.port,
	}
	s.udpconn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return
	}

	return
}

func (s *Server) SendUDP() {
	for {
		msg := <-s.udpsend
		// Encrypted
		if msg.client != nil {
			crypted := make([]byte, len(msg.buf)+4)
			msg.client.crypt.Encrypt(crypted, msg.buf)
			s.udpconn.WriteTo(crypted, msg.client.udpaddr)
			// Non-encrypted
		} else if msg.address != nil {
			s.udpconn.WriteTo(msg.buf, msg.address)
		} else {
			// Skipping
		}
	}
}

// Listen for and handle UDP packets.
func (server *Server) ListenUDP() {
	buf := make([]byte, UDPPacketSize)
	for {
		nread, remote, err := server.udpconn.ReadFrom(buf)
		if err != nil {
			// Not much to do here. This is bad, of course. Should we panic this server instance?
			continue
		}

		udpaddr, ok := remote.(*net.UDPAddr)
		if !ok {
			server.Printf("No UDPAddr in read packet. Disabling UDP. (Windows?)")
			return
		}

		// Length 12 is for ping datagrams from the ConnectDialog.
		if nread == 12 {
			readbuf := bytes.NewBuffer(buf)
			var (
				tmp32 uint32
				rand  uint64
			)
			_ = binary.Read(readbuf, binary.BigEndian, &tmp32)
			_ = binary.Read(readbuf, binary.BigEndian, &rand)

			buffer := bytes.NewBuffer(make([]byte, 0, 24))
			_ = binary.Write(buffer, binary.BigEndian, uint32((1<<16)|(2<<8)|2))
			_ = binary.Write(buffer, binary.BigEndian, rand)
			_ = binary.Write(buffer, binary.BigEndian, uint32(len(server.clients)))
			_ = binary.Write(buffer, binary.BigEndian, server.cfg.Uint32Value("MaxUsers"))
			_ = binary.Write(buffer, binary.BigEndian, server.cfg.Uint32Value("MaxBandwidth"))

			server.udpsend <- &Message{
				buf:     buffer.Bytes(),
				address: udpaddr,
			}
		} else {
			server.handleUdpPacket(udpaddr, buf, nread)
		}
	}
}

func (server *Server) handleUdpPacket(udpaddr *net.UDPAddr, buf []byte, nread int) {
	var match *Client
	plain := make([]byte, nread-4)

	// Determine which client sent the the packet.  First, we
	// check the map 'hpclients' in the server struct. It maps
	// a hort-post combination to a client.
	//
	// If we don't find any matches, we look in the 'hclients',
	// which maps a host address to a slice of clients.
	server.hmutex.Lock()
	defer server.hmutex.Unlock()
	client, ok := server.hpclients[udpaddr.String()]
	if ok {
		err := client.crypt.Decrypt(plain[0:], buf[0:nread])
		if err != nil {
			client.cryptResync()
			return
		}
		match = client
	} else {
		host := udpaddr.IP.String()
		hostclients := server.hclients[host]
		for _, client := range hostclients {
			err := client.crypt.Decrypt(plain[0:], buf[0:nread])
			if err != nil {
				client.cryptResync()
				return
			} else {
				match = client
			}
		}
		if match != nil {
			match.udpaddr = udpaddr
			server.hpclients[udpaddr.String()] = match
		}
	}

	if match == nil {
		return
	}

	match.udp = true
	match.udprecv <- plain
}

// Clear the ACL cache
func (s *Server) ClearACLCache() {
	s.aclcache = NewACLCache()
}

// Helper method for users entering new channels
func (server *Server) userEnterChannel(client *Client, channel *Channel, userstate *mumbleproto.UserState) {
	if client.Channel == channel {
		return
	}

	oldchan := client.Channel
	if oldchan != nil {
		oldchan.RemoveClient(client)
	}
	channel.AddClient(client)

	server.ClearACLCache()
	// fixme(mkrautz): Set LastChannel for user in datastore
	// fixme(mkrautz): Remove channel if temporary

	canspeak := server.HasPermission(client, channel, SpeakPermission)
	if canspeak == client.Suppress {
		client.Suppress = !canspeak
		userstate.Suppress = proto.Bool(client.Suppress)
	}

	server.sendClientPermissions(client, channel)
	if channel.parent != nil {
		server.sendClientPermissions(client, channel.parent)
	}
}

// Register a client on the server.
func (s *Server) RegisterClient(client *Client) (uid uint32, err error) {
	// Increment nextUserId only if registration succeeded.
	defer func() {
		if err == nil {
			s.nextUserId += 1
		}
	}()

	user, err := NewUser(s.nextUserId, client.Username)
	if err != nil {
		return 0, err
	}

	// Grumble can only register users with certificates.
	if len(client.CertHash) == 0 {
		return 0, errors.New("no cert hash")
	}

	user.Email = client.Email
	user.CertHash = client.CertHash

	uid = s.nextUserId
	s.Users[uid] = user
	s.UserCertMap[client.CertHash] = user
	s.UserNameMap[client.Username] = user

	return uid, nil
}

// Remove a registered user.
func (s *Server) RemoveRegistration(uid uint32) (err error) {
	user, ok := s.Users[uid]
	if !ok {
		return errors.New("Unknown user ID")
	}

	// Remove from user maps
	delete(s.Users, uid)
	delete(s.UserCertMap, user.CertHash)
	delete(s.UserNameMap, user.Name)

	// Remove from groups and ACLs.
	s.removeRegisteredUserFromChannel(uid, s.RootChannel())

	return nil
}

// Remove references for user id uid from channel. Traverses subchannels.
func (s *Server) removeRegisteredUserFromChannel(uid uint32, channel *Channel) {

	newACL := []*ChannelACL{}
	for _, chanacl := range channel.ACL {
		if chanacl.UserId == int(uid) {
			continue
		}
		newACL = append(newACL, chanacl)
	}
	channel.ACL = newACL

	for _, grp := range channel.Groups {
		if _, ok := grp.Add[int(uid)]; ok {
			delete(grp.Add, int(uid))
		}
		if _, ok := grp.Remove[int(uid)]; ok {
			delete(grp.Remove, int(uid))
		}
		if _, ok := grp.Temporary[int(uid)]; ok {
			delete(grp.Temporary, int(uid))
		}
	}

	for _, subChan := range channel.children {
		s.removeRegisteredUserFromChannel(uid, subChan)
	}
}

// Remove a channel
func (server *Server) RemoveChannel(channel *Channel) {
	// Can't remove root
	if channel.parent == nil {
		return
	}

	// Remove all links
	for _, linkedChannel := range channel.Links {
		delete(linkedChannel.Links, channel.Id)
	}

	// Remove all subchannels
	for _, subChannel := range channel.children {
		server.RemoveChannel(subChannel)
	}

	// Remove all clients
	for _, client := range channel.clients {
		target := channel.parent
		for target.parent != nil && !server.HasPermission(client, target, EnterPermission) {
			target = target.parent
		}

		userstate := &mumbleproto.UserState{}
		userstate.Session = proto.Uint32(client.Session)
		userstate.ChannelId = proto.Uint32(uint32(target.Id))
		server.userEnterChannel(client, target, userstate)
		if err := server.broadcastProtoMessage(userstate); err != nil {
			server.Panicf("%v", err)
		}
	}

	// Remove the channel itself
	parent := channel.parent
	delete(parent.children, channel.Id)
	delete(server.Channels, channel.Id)
	chanremove := &mumbleproto.ChannelRemove{
		ChannelId: proto.Uint32(uint32(channel.Id)),
	}
	if err := server.broadcastProtoMessage(chanremove); err != nil {
		server.Panicf("%v", err)
	}
}

// Is the incoming connection conn banned?
func (server *Server) IsBanned(conn net.Conn) bool {
	server.banlock.RLock()
	defer server.banlock.RUnlock()

	for _, ban := range server.Bans {
		addr := conn.RemoteAddr().(*net.TCPAddr)
		if ban.Match(addr.IP) && !ban.IsExpired() {
			return true
		}
	}

	return false
}

// Filter incoming text according to the server's current rules.
func (server *Server) FilterText(text string) (filtered string, err error) {
	options := &htmlfilter.Options{
		StripHTML:             !server.cfg.BoolValue("AllowHTML"),
		MaxTextMessageLength:  server.cfg.IntValue("MaxTextMessageLength"),
		MaxImageMessageLength: server.cfg.IntValue("MaxImageMessageLength"),
	}
	return htmlfilter.Filter(text, options)
}

// The accept loop of the server.
func (s *Server) ListenAndMurmur() {
	// Launch the event handler goroutine
	go s.handler()

	host := s.cfg.StringValue("Address")
	if host != "" {
		s.address = host
	}
	port := s.cfg.IntValue("Port")
	if port != 0 {
		s.port = port
	}

	s.running = true

	// Setup our UDP listener and spawn our reader and writer goroutines
	s.SetupUDP()
	go s.ListenUDP()
	go s.SendUDP()

	// Create a new listening TLS socket.
	cert, err := tls.LoadX509KeyPair(filepath.Join(Args.DataDir, "cert"), filepath.Join(Args.DataDir, "key"))
	if err != nil {
		s.Printf("Unable to load x509 key pair: %v", err)
		return
	}

	cfg := new(tls.Config)
	cfg.Certificates = append(cfg.Certificates, cert)
	cfg.AuthenticateClient = true
	s.tlscfg = cfg

	tl, err := net.ListenTCP("tcp", &net.TCPAddr{
		net.ParseIP(s.address),
		s.port,
	})
	if err != nil {
		s.Printf("Cannot bind: %s\n", err)
		return
	}

	listener := tls.NewListener(tl, s.tlscfg)

	s.Printf("Started: listening on %v", tl.Addr())

	// Open a fresh freezer log
	err = s.openFreezeLog()
	if err != nil {
		s.Fatal(err)
	}

	// Update server registration if needed.
	go func() {
		time.Sleep((60 + s.Id*10) * 1e9)
		s.RegisterPublicServer()
	}()

	// The main accept loop. Basically, we block
	// until we get a new client connection, and
	// when we do get a new connection, we spawn
	// a new Go-routine to handle the client.
	for {
		// New client connected
		conn, err := listener.Accept()
		if err != nil {
			s.Printf("Unable to accept new client: %v", err)
			continue
		}

		// Is the client banned?
		// fixme(mkrautz): Clean up expired bans
		if s.IsBanned(conn) {
			s.Printf("Rejected client %v: Banned", conn.RemoteAddr())
			err := conn.Close()
			if err != nil {
				s.Printf("Unable to close connection: %v", err)
			}
			continue
		}

		// Create a new client connection from our *tls.Conn
		// which wraps net.TCPConn.
		err = s.NewClient(conn)
		if err != nil {
			s.Printf("Unable to handle new client: %v", err)
			continue
		}
	}
}
