package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/vincent-petithory/dataurl"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type Values struct {
	m map[string]string
}

func (v Values) Get(key string) string {
	return v.m[key]
}

var messageTypes = []string{
	"Message",
	"ReadReceipt",
	"Presence",
	"HistorySync",
	"ChatPresence",
	"All",
}

func (s *server) authalice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		var ctx context.Context
		userid := 0
		txtid := ""
		webhook := ""
		jid := ""
		events := ""

		// Get token from headers or uri parameters
		token := r.Header.Get("token")
		if token == "" {
			token = strings.Join(r.URL.Query()["token"], "")
		}

		myuserinfo, found := userinfocache.Get(token)
		if !found {
			log.Info().Msg("Looking for user information in DB")
			// Checks DB from matching user and store user values in context
			rows, err := s.db.Query(
				"SELECT id,webhook,jid,events FROM users WHERE token=? LIMIT 1",
				token,
			)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			defer rows.Close()
			for rows.Next() {
				err = rows.Scan(&txtid, &webhook, &jid, &events)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, err)
					return
				}
				userid, _ = strconv.Atoi(txtid)
				v := Values{map[string]string{
					"Id":      txtid,
					"Jid":     jid,
					"Webhook": webhook,
					"Token":   token,
					"Events":  events,
				}}

				userinfocache.Set(token, v, cache.NoExpiration)
				ctx = context.WithValue(r.Context(), "userinfo", v)
			}
		} else {
			ctx = context.WithValue(r.Context(), "userinfo", myuserinfo)
			userid, _ = strconv.Atoi(myuserinfo.(Values).Get("Id"))
		}

		if userid == 0 {
			s.Respond(w, r, http.StatusUnauthorized, errors.New("Unauthorized"))
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware: Authenticate connections based on Token header/uri parameter
func (s *server) auth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		var ctx context.Context
		userid := 0
		txtid := ""
		webhook := ""
		jid := ""
		events := ""

		// Get token from headers or uri parameters
		token := r.Header.Get("token")
		if token == "" {
			token = strings.Join(r.URL.Query()["token"], "")
		}

		myuserinfo, found := userinfocache.Get(token)
		if !found {
			log.Info().Msg("Looking for user information in DB")
			// Checks DB from matching user and store user values in context
			rows, err := s.db.Query(
				"SELECT id,webhook,jid,events FROM users WHERE token=? LIMIT 1",
				token,
			)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			defer rows.Close()
			for rows.Next() {
				err = rows.Scan(&txtid, &webhook, &jid, &events)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, err)
					return
				}
				userid, _ = strconv.Atoi(txtid)
				v := Values{map[string]string{
					"Id":      txtid,
					"Jid":     jid,
					"Webhook": webhook,
					"Token":   token,
					"Events":  events,
				}}

				userinfocache.Set(token, v, cache.NoExpiration)
				ctx = context.WithValue(r.Context(), "userinfo", v)
			}
		} else {
			ctx = context.WithValue(r.Context(), "userinfo", myuserinfo)
			userid, _ = strconv.Atoi(myuserinfo.(Values).Get("Id"))
		}

		if userid == 0 {
			s.Respond(w, r, http.StatusUnauthorized, errors.New("Unauthorized"))
			return
		}
		handler(w, r.WithContext(ctx))
	}
}

// Connects to Whatsapp Servers
func (s *server) Connect() http.HandlerFunc {

	type connectStruct struct {
		Subscribe []string
		Immediate bool
	}

	return func(w http.ResponseWriter, r *http.Request) {

		webhook := r.Context().Value("userinfo").(Values).Get("Webhook")
		jid := r.Context().Value("userinfo").(Values).Get("Jid")
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")
		userid, _ := strconv.Atoi(txtid)
		eventstring := ""

		// Decodes request BODY looking for events to subscribe
		decoder := json.NewDecoder(r.Body)
		var t connectStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if clientPointer[userid] != nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("already connected"))
			return
		} else {

			var subscribedEvents []string
			if len(t.Subscribe) < 1 {
				if !Find(subscribedEvents, "All") {
					subscribedEvents = append(subscribedEvents, "All")
				}
			} else {
				for _, arg := range t.Subscribe {
					if !Find(messageTypes, arg) {
						log.Warn().Str("Type", arg).Msg("Message type discarded")
						continue
					}
					if !Find(subscribedEvents, arg) {
						subscribedEvents = append(subscribedEvents, arg)
					}
				}
			}
			eventstring = strings.Join(subscribedEvents, ",")
			_, err = s.db.Exec("UPDATE users SET events=? WHERE id=?", eventstring, userid)
			if err != nil {
				log.Warn().Msg("Could not set events in users table")
			}
			log.Info().Str("events", eventstring).Msg("Setting subscribed events")
			v := updateUserInfo(r.Context().Value("userinfo"), "Events", eventstring)
			userinfocache.Set(token, v, cache.NoExpiration)

			log.Info().Str("jid", jid).Msg("Attempt to connect")
			killchannel[userid] = make(chan bool)
			go s.startClient(userid, jid, token, subscribedEvents)

			if !t.Immediate {
				log.Warn().Msg("Waiting 10 seconds")
				time.Sleep(10000 * time.Millisecond)

				if clientPointer[userid] != nil {
					if !clientPointer[userid].IsConnected() {
						s.Respond(w, r, http.StatusInternalServerError, errors.New("failed to connect"))
						return
					}
				} else {
					s.Respond(w, r, http.StatusInternalServerError, errors.New("failed to connect"))
					return
				}
			}
		}

		response := map[string]interface{}{
			"webhook": webhook,
			"jid":     jid,
			"events":  eventstring,
			"details": "Connected!",
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		} else {
			s.Respond(w, r, http.StatusOK, string(responseJson))
			return
		}
	}
}

// Disconnects from Whatsapp websocket, does not log out device
func (s *server) Disconnect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		jid := r.Context().Value("userinfo").(Values).Get("Jid")
		token := r.Context().Value("userinfo").(Values).Get("Token")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}
		if clientPointer[userid].IsConnected() {
			if clientPointer[userid].IsLoggedIn() {
				log.Info().Str("jid", jid).Msg("Disconnection successfull")
				killchannel[userid] <- true
				_, err := s.db.Exec("UPDATE users SET events=? WHERE id=?", "", userid)
				if err != nil {
					log.Warn().Str("userid", txtid).Msg("Could not set events in users table")
				}
				v := updateUserInfo(r.Context().Value("userinfo"), "Events", "")
				userinfocache.Set(token, v, cache.NoExpiration)

				response := map[string]interface{}{"Details": "Disconnected"}
				responseJson, err := json.Marshal(response)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, err)
				} else {
					s.Respond(w, r, http.StatusOK, string(responseJson))
				}
				return
			} else {
				log.Warn().Str("jid", jid).Msg("Ignoring disconnect as it was not connected")
				s.Respond(w, r, http.StatusInternalServerError, errors.New("cannot disconnect because it is not logged in"))
				return
			}
		} else {
			log.Warn().Str("jid", jid).Msg("Ignoring disconnect as it was not connected")
			s.Respond(w, r, http.StatusInternalServerError, errors.New("cannot disconnect because it is not logged in"))
			return
		}
	}
}

// Gets WebHook
func (s *server) GetWebhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		webhook := ""
		events := ""
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		rows, err := s.db.Query("SELECT webhook,events FROM users WHERE id=? LIMIT 1", txtid)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("could not get webhook: %v", err),
			)
			return
		}
		defer rows.Close()
		for rows.Next() {
			err = rows.Scan(&webhook, &events)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusInternalServerError,
					fmt.Errorf("could not get webhook: %s", fmt.Sprintf("%s", err)),
				)
				return
			}
		}
		err = rows.Err()
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("could not get webhook: %s", fmt.Sprintf("%s", err)),
			)
			return
		}

		eventarray := strings.Split(events, ",")

		response := map[string]interface{}{"webhook": webhook, "subscribe": eventarray}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sets WebHook
func (s *server) SetWebhook() http.HandlerFunc {
	type webhookStruct struct {
		WebhookURL string
	}
	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")
		userid, _ := strconv.Atoi(txtid)

		decoder := json.NewDecoder(r.Body)
		var t webhookStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("could not set webhook: %v", err),
			)
			return
		}
		var webhook = t.WebhookURL

		_, err = s.db.Exec("UPDATE users SET webhook=? WHERE id=?", webhook, userid)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("%s", err))
			return
		}

		v := updateUserInfo(r.Context().Value("userinfo"), "Webhook", webhook)
		userinfocache.Set(token, v, cache.NoExpiration)

		response := map[string]interface{}{"webhook": webhook}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Gets QR code encoded in Base64
func (s *server) GetQR() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		code := ""

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		} else {
			if !clientPointer[userid].IsConnected() {
				s.Respond(w, r, http.StatusInternalServerError, errors.New("not connected"))
				return
			}
			rows, err := s.db.Query("SELECT qrcode AS code FROM users WHERE id=? LIMIT 1", userid)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			defer rows.Close()
			for rows.Next() {
				err = rows.Scan(&code)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, err)
					return
				}
			}
			err = rows.Err()
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			if clientPointer[userid].IsLoggedIn() {
				s.Respond(w, r, http.StatusInternalServerError, errors.New("already loggedin"))
				return
			}
		}

		log.Info().Str("userid", txtid).Str("qrcode", code).Msg("Get QR successful")
		response := map[string]interface{}{"QRCode": code}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Logs out device from Whatsapp (requires to scan QR next time)
func (s *server) Logout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		jid := r.Context().Value("userinfo").(Values).Get("Jid")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		} else {
			if clientPointer[userid].IsLoggedIn() && clientPointer[userid].IsConnected() {
				err := clientPointer[userid].Logout()
				if err != nil {
					log.Error().Str("jid", jid).Msg("Could not perform logout")
					s.Respond(w, r, http.StatusInternalServerError, errors.New("could not perform logout"))
					return
				} else {
					log.Info().Str("jid", jid).Msg("Logged out")
					killchannel[userid] <- true
				}
			} else {
				if clientPointer[userid].IsConnected() {
					log.Warn().Str("jid", jid).Msg("Ignoring logout as it was not logged in")
					s.Respond(w, r, http.StatusInternalServerError, errors.New("could not disconnect as it was not logged in"))
					return
				} else {
					log.Warn().Str("jid", jid).Msg("Ignoring logout as it was not connected")
					s.Respond(w, r, http.StatusInternalServerError, errors.New("could not disconnect as it was not connected"))
					return
				}
			}
		}

		response := map[string]interface{}{"Details": "Logged out"}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Gets Connected and LoggedIn Status
func (s *server) GetStatus() http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		isConnected := clientPointer[userid].IsConnected()
		isLoggedIn := clientPointer[userid].IsLoggedIn()

		response := map[string]interface{}{"Connected": isConnected, "LoggedIn": isLoggedIn}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends a document/attachment message
func (s *server) SendDocument() http.HandlerFunc {

	type documentStruct struct {
		Phone       string
		Document    string
		FileName    string
		Id          string
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		msgid := ""
		var resp whatsmeow.SendResponse

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t documentStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Document == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing document in payload"))
			return
		}

		if t.FileName == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing filename in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		if t.Document[0:29] == "data:application/octet-stream" {
			dataURL, err := dataurl.DecodeString(t.Document)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
				uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaDocument)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to upload file: %v", err))
					return
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("document data should start with \"data:application/octet-stream;base64,\""))
			return
		}

		msg := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			Url:           proto.String(uploaded.URL),
			FileName:      &t.FileName,
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(filedata)),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(filedata))),
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends an audio message
func (s *server) SendAudio() http.HandlerFunc {

	type audioStruct struct {
		Phone       string
		Audio       string
		Caption     string
		Id          string
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		msgid := ""
		var resp whatsmeow.SendResponse

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t audioStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Audio == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing audio in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		if t.Audio[0:14] == "data:audio/ogg" {
			dataURL, err := dataurl.DecodeString(t.Audio)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
				uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaAudio)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to upload file: %v", err))
					return
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("audio data should start with \"data:audio/ogg;base64,\""))
			return
		}

		ptt := true
		mime := "audio/ogg; codecs=opus"

		msg := &waProto.Message{AudioMessage: &waProto.AudioMessage{
			Url:        proto.String(uploaded.URL),
			DirectPath: proto.String(uploaded.DirectPath),
			MediaKey:   uploaded.MediaKey,
			//Mimetype:      proto.String(http.DetectContentType(filedata)),
			Mimetype:      &mime,
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(filedata))),
			Ptt:           &ptt,
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends an Image message
func (s *server) SendImage() http.HandlerFunc {

	type imageStruct struct {
		Phone       string
		Image       string
		Caption     string
		Id          string
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		msgid := ""
		var resp whatsmeow.SendResponse

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t imageStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Image == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing image in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		if t.Image[0:10] == "data:image" {
			dataURL, err := dataurl.DecodeString(t.Image)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
				uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaImage)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to upload file: %v", err))
					return
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("image data should start with \"data:image/png;base64,\""))
			return
		}

		msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
			Caption:       proto.String(t.Caption),
			Url:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(filedata)),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(filedata))),
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends Sticker message
func (s *server) SendSticker() http.HandlerFunc {

	type stickerStruct struct {
		Phone        string
		Sticker      string
		Id           string
		PngThumbnail []byte
		ContextInfo  waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		msgid := ""
		var resp whatsmeow.SendResponse

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t stickerStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Sticker == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing sticker in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		if t.Sticker[0:4] == "data" {
			dataURL, err := dataurl.DecodeString(t.Sticker)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
				uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaImage)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to upload file: %v", err))
					return
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("data should start with \"data:mime/type;base64,\""))
			return
		}

		msg := &waProto.Message{StickerMessage: &waProto.StickerMessage{
			Url:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(filedata)),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(filedata))),
			PngThumbnail:  t.PngThumbnail,
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends Video message
func (s *server) SendVideo() http.HandlerFunc {

	type imageStruct struct {
		Phone         string
		Video         string
		Caption       string
		Id            string
		JpegThumbnail []byte
		ContextInfo   waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		msgid := ""
		var resp whatsmeow.SendResponse

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t imageStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Video == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing video in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		if t.Video[0:4] == "data" {
			dataURL, err := dataurl.DecodeString(t.Video)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
				uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaVideo)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to upload file: %v", err))
					return
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("data should start with \"data:mime/type;base64,\""))
			return
		}

		msg := &waProto.Message{VideoMessage: &waProto.VideoMessage{
			Caption:       proto.String(t.Caption),
			Url:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(filedata)),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(filedata))),
			JpegThumbnail: t.JpegThumbnail,
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends Contact
func (s *server) SendContact() http.HandlerFunc {

	type contactStruct struct {
		Phone       string
		Id          string
		Name        string
		Vcard       string
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t contactStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}
		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}
		if t.Name == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing name in payload"))
			return
		}
		if t.Vcard == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing vcard in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		msg := &waProto.Message{ContactMessage: &waProto.ContactMessage{
			DisplayName: &t.Name,
			Vcard:       &t.Vcard,
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends location
func (s *server) SendLocation() http.HandlerFunc {

	type locationStruct struct {
		Phone       string
		Id          string
		Name        string
		Latitude    float64
		Longitude   float64
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t locationStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}
		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}
		if t.Latitude == 0 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing latitude in payload"))
			return
		}
		if t.Longitude == 0 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing longitude in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		msg := &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude:  &t.Latitude,
			DegreesLongitude: &t.Longitude,
			Name:             &t.Name,
		}}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends Buttons (not implemented, does not work)

func (s *server) SendButtons() http.HandlerFunc {

	type buttonStruct struct {
		ButtonId   string
		ButtonText string
	}
	type textStruct struct {
		Phone   string
		Title   string
		Buttons []buttonStruct
		Id      string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t textStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Title == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing title in payload"))
			return
		}

		if len(t.Buttons) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing Buttons in Payload"))
			return
		}
		if len(t.Buttons) > 3 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("buttons cant more than 3"))
			return
		}

		recipient, ok := parseJID(t.Phone)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse phone"))
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var buttons []*waProto.ButtonsMessage_Button

		for _, item := range t.Buttons {
			buttons = append(buttons, &waProto.ButtonsMessage_Button{
				ButtonId: proto.String(item.ButtonId),
				ButtonText: &waProto.ButtonsMessage_Button_ButtonText{
					DisplayText: proto.String(item.ButtonText),
				},
				Type:           waProto.ButtonsMessage_Button_RESPONSE.Enum(),
				NativeFlowInfo: &waProto.ButtonsMessage_Button_NativeFlowInfo{},
			})
		}

		msg2 := &waProto.ButtonsMessage{
			ContentText: proto.String(t.Title),
			HeaderType:  waProto.ButtonsMessage_EMPTY.Enum(),
			Buttons:     buttons,
		}

		resp, err = clientPointer[userid].SendMessage(
			context.Background(),
			recipient,
			&waProto.Message{ViewOnceMessage: &waProto.FutureProofMessage{
				Message: &waProto.Message{
					ButtonsMessage: msg2,
				},
			}},
		)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// SendList
// https://github.com/tulir/whatsmeow/issues/305
func (s *server) SendList() http.HandlerFunc {

	type rowsStruct struct {
		RowId       string
		Title       string
		Description string
	}

	type sectionsStruct struct {
		Title string
		Rows  []rowsStruct
	}

	type listStruct struct {
		Phone       string
		Title       string
		Description string
		ButtonText  string
		FooterText  string
		Sections    []sectionsStruct
		Id          string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t listStruct
		err := decoder.Decode(&t)
		marshal, _ := json.Marshal(t)
		fmt.Println(string(marshal))
		if err != nil {
			fmt.Println(err)
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode Payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing Phone in Payload"))
			return
		}

		if t.Title == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing Title in Payload"))
			return
		}

		if t.Description == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing Description in Payload"))
			return
		}

		if t.ButtonText == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing ButtonText in Payload"))
			return
		}

		if len(t.Sections) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing Sections in Payload"))
			return
		}
		recipient, ok := parseJID(t.Phone)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse Phone"))
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		var sections []*waProto.ListMessage_Section

		for _, item := range t.Sections {
			var rows []*waProto.ListMessage_Row
			id := 1
			for _, row := range item.Rows {
				var idtext string
				if row.RowId == "" {
					idtext = strconv.Itoa(id)
				} else {
					idtext = row.RowId
				}
				rows = append(rows, &waProto.ListMessage_Row{
					RowId:       proto.String(idtext),
					Title:       proto.String(row.Title),
					Description: proto.String(row.Description),
				})
			}

			sections = append(sections, &waProto.ListMessage_Section{
				Title: proto.String(item.Title),
				Rows:  rows,
			})
		}
		msg1 := &waProto.ListMessage{
			Title:       proto.String(t.Title),
			Description: proto.String(t.Description),
			ButtonText:  proto.String(t.ButtonText),
			ListType:    waProto.ListMessage_SINGLE_SELECT.Enum(),
			Sections:    sections,
			FooterText:  proto.String(t.FooterText),
		}

		resp, err = clientPointer[userid].SendMessage(
			context.Background(),
			recipient,
			&waProto.Message{
				ViewOnceMessage: &waProto.FutureProofMessage{
					Message: &waProto.Message{
						ListMessage: msg1,
					},
				}},
		)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sends a regular text message
func (s *server) SendMessage() http.HandlerFunc {

	type textStruct struct {
		Phone       string
		Body        string
		Id          string
		ContextInfo waProto.ContextInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t textStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Body == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing body in payload"))
			return
		}

		recipient, err := validateMessageFields(
			t.Phone,
			t.ContextInfo.StanzaId,
			t.ContextInfo.Participant,
		)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		if t.Id == "" {
			msgid = whatsmeow.GenerateMessageID()
		} else {
			msgid = t.Id
		}

		//	msg := &waProto.Message{Conversation: &t.Body}

		msg := &waProto.Message{
			ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text: &t.Body,
			},
		}

		if t.ContextInfo.StanzaId != nil {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaId:      proto.String(*t.ContextInfo.StanzaId),
				Participant:   proto.String(*t.ContextInfo.Participant),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// checks if users/phones are on Whatsapp
func (s *server) CheckUser() http.HandlerFunc {

	type checkUserStruct struct {
		Phone []string
	}

	type User struct {
		Query        string
		IsInWhatsapp bool
		JID          string
		VerifiedName string
	}

	type UserCollection struct {
		Users []User
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t checkUserStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if len(t.Phone) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		resp, err := clientPointer[userid].IsOnWhatsApp(t.Phone)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("failed to check if users are on whatsapp: %s", err),
			)
			return
		}

		uc := new(UserCollection)
		for _, item := range resp {
			if item.VerifiedName != nil {
				var msg = User{Query: item.Query, IsInWhatsapp: item.IsIn, JID: item.JID.String(), VerifiedName: item.VerifiedName.Details.GetVerifiedName()}
				uc.Users = append(uc.Users, msg)
			} else {
				var msg = User{Query: item.Query, IsInWhatsapp: item.IsIn, JID: item.JID.String(), VerifiedName: ""}
				uc.Users = append(uc.Users, msg)
			}
		}
		responseJson, err := json.Marshal(uc)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Gets user information
func (s *server) GetUser() http.HandlerFunc {

	type checkUserStruct struct {
		Phone []string
	}

	type UserCollection struct {
		Users map[types.JID]types.UserInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t checkUserStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if len(t.Phone) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		var jids []types.JID
		for _, arg := range t.Phone {
			jid, ok := parseJID(arg)
			if !ok {
				return
			}
			jids = append(jids, jid)
		}
		resp, err := clientPointer[userid].GetUserInfo(jids)

		if err != nil {
			msg := fmt.Sprintf("Failed to get user info: %v", err)
			log.Error().Msg(msg)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		uc := new(UserCollection)
		uc.Users = make(map[types.JID]types.UserInfo)

		for jid, info := range resp {
			uc.Users[jid] = info
		}

		responseJson, err := json.Marshal(uc)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Gets avatar info for user
func (s *server) GetAvatar() http.HandlerFunc {

	type getAvatarStruct struct {
		Phone   string
		Preview bool
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t getAvatarStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if len(t.Phone) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		jid, ok := parseJID(t.Phone)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse phone"))
			return
		}

		var pic *types.ProfilePictureInfo

		existingID := ""
		pic, err = clientPointer[userid].GetProfilePictureInfo(
			jid,
			&whatsmeow.GetProfilePictureParams{
				Preview:    t.Preview,
				ExistingID: existingID,
			},
		)
		if err != nil {
			msg := fmt.Sprintf("Failed to get avatar: %v", err)
			log.Error().Msg(msg)
			s.Respond(w, r, http.StatusInternalServerError, errors.New(msg))
			return
		}

		if pic == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no avatar found"))
			return
		}

		log.Info().Str("id", pic.ID).Str("url", pic.URL).Msg("Got avatar")

		responseJson, err := json.Marshal(pic)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Gets all contacts
func (s *server) GetContacts() http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		result, err := clientPointer[userid].Store.Contacts.GetAllContacts()
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		responseJson, err := json.Marshal(result)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Sets Chat Presence (typing/paused/recording audio)
func (s *server) ChatPresence() http.HandlerFunc {

	type chatPresenceStruct struct {
		Phone string
		State string
		Media types.ChatPresenceMedia
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t chatPresenceStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if len(t.Phone) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if len(t.State) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing state in payload"))
			return
		}

		jid, ok := parseJID(t.Phone)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse phone"))
			return
		}

		err = clientPointer[userid].SendChatPresence(
			jid,
			types.ChatPresence(t.State),
			types.ChatPresenceMedia(t.Media),
		)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				errors.New("failure sending chat presence to whatsapp servers"),
			)
			return
		}

		response := map[string]interface{}{"Details": "Chat presence set successfuly"}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Downloads Image and returns base64 representation
func (s *server) DownloadImage() http.HandlerFunc {

	type downloadImageStruct struct {
		Url           string
		DirectPath    string
		MediaKey      []byte
		Mimetype      string
		FileEncSHA256 []byte
		FileSHA256    []byte
		FileLength    uint64
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		mimetype := ""
		var imgdata []byte

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		// check/creates user directory for files
		userDirectory := fmt.Sprintf("%s/files/user_%s", s.exPath, txtid)
		_, err := os.Stat(userDirectory)
		if os.IsNotExist(err) {
			errDir := os.MkdirAll(userDirectory, 0751)
			if errDir != nil {
				s.Respond(
					w,
					r,
					http.StatusInternalServerError,
					fmt.Errorf("could not create user directory (%s)", userDirectory),
				)
				return
			}
		}

		decoder := json.NewDecoder(r.Body)
		var t downloadImageStruct
		err = decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
			Url:           proto.String(t.Url),
			DirectPath:    proto.String(t.DirectPath),
			MediaKey:      t.MediaKey,
			Mimetype:      proto.String(t.Mimetype),
			FileEncSha256: t.FileEncSHA256,
			FileSha256:    t.FileSHA256,
			FileLength:    &t.FileLength,
		}}

		img := msg.GetImageMessage()

		if img != nil {
			imgdata, err = clientPointer[userid].Download(img)
			if err != nil {
				log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to download image")
				msg := fmt.Sprintf("Failed to download image %v", err)
				s.Respond(w, r, http.StatusInternalServerError, errors.New(msg))
				return
			}
			mimetype = img.GetMimetype()
		}

		dataURL := dataurl.New(imgdata, mimetype)
		response := map[string]interface{}{"Mimetype": mimetype, "Data": dataURL.String()}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Downloads Document and returns base64 representation
func (s *server) DownloadDocument() http.HandlerFunc {

	type downloadDocumentStruct struct {
		Url           string
		DirectPath    string
		MediaKey      []byte
		Mimetype      string
		FileEncSHA256 []byte
		FileSHA256    []byte
		FileLength    uint64
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		mimetype := ""
		var docdata []byte

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		// check/creates user directory for files
		userDirectory := fmt.Sprintf("%s/files/user_%s", s.exPath, txtid)
		_, err := os.Stat(userDirectory)
		if os.IsNotExist(err) {
			errDir := os.MkdirAll(userDirectory, 0751)
			if errDir != nil {
				s.Respond(
					w,
					r,
					http.StatusInternalServerError,
					fmt.Errorf("could not create user directory (%s)", userDirectory),
				)
				return
			}
		}

		decoder := json.NewDecoder(r.Body)
		var t downloadDocumentStruct
		err = decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		msg := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			Url:           proto.String(t.Url),
			DirectPath:    proto.String(t.DirectPath),
			MediaKey:      t.MediaKey,
			Mimetype:      proto.String(t.Mimetype),
			FileEncSha256: t.FileEncSHA256,
			FileSha256:    t.FileSHA256,
			FileLength:    &t.FileLength,
		}}

		doc := msg.GetDocumentMessage()

		if doc != nil {
			docdata, err = clientPointer[userid].Download(doc)
			if err != nil {
				log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to download document")
				msg := fmt.Sprintf("Failed to download document %v", err)
				s.Respond(w, r, http.StatusInternalServerError, errors.New(msg))
				return
			}
			mimetype = doc.GetMimetype()
		}

		dataURL := dataurl.New(docdata, mimetype)
		response := map[string]interface{}{"Mimetype": mimetype, "Data": dataURL.String()}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Downloads Video and returns base64 representation
func (s *server) DownloadVideo() http.HandlerFunc {

	type downloadVideoStruct struct {
		Url           string
		DirectPath    string
		MediaKey      []byte
		Mimetype      string
		FileEncSHA256 []byte
		FileSHA256    []byte
		FileLength    uint64
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		mimetype := ""
		var docdata []byte

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		// check/creates user directory for files
		userDirectory := fmt.Sprintf("%s/files/user_%s", s.exPath, txtid)
		_, err := os.Stat(userDirectory)
		if os.IsNotExist(err) {
			errDir := os.MkdirAll(userDirectory, 0751)
			if errDir != nil {
				s.Respond(
					w,
					r,
					http.StatusInternalServerError,
					fmt.Errorf("could not create user directory (%s)", userDirectory),
				)
				return
			}
		}

		decoder := json.NewDecoder(r.Body)
		var t downloadVideoStruct
		err = decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		msg := &waProto.Message{VideoMessage: &waProto.VideoMessage{
			Url:           proto.String(t.Url),
			DirectPath:    proto.String(t.DirectPath),
			MediaKey:      t.MediaKey,
			Mimetype:      proto.String(t.Mimetype),
			FileEncSha256: t.FileEncSHA256,
			FileSha256:    t.FileSHA256,
			FileLength:    &t.FileLength,
		}}

		doc := msg.GetVideoMessage()

		if doc != nil {
			docdata, err = clientPointer[userid].Download(doc)
			if err != nil {
				log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to download video")
				msg := fmt.Sprintf("Failed to download video %v", err)
				s.Respond(w, r, http.StatusInternalServerError, errors.New(msg))
				return
			}
			mimetype = doc.GetMimetype()
		}

		dataURL := dataurl.New(docdata, mimetype)
		response := map[string]interface{}{"Mimetype": mimetype, "Data": dataURL.String()}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Downloads Audio and returns base64 representation
func (s *server) DownloadAudio() http.HandlerFunc {

	type downloadAudioStruct struct {
		Url           string
		DirectPath    string
		MediaKey      []byte
		Mimetype      string
		FileEncSHA256 []byte
		FileSHA256    []byte
		FileLength    uint64
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		mimetype := ""
		var docdata []byte

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		// check/creates user directory for files
		userDirectory := fmt.Sprintf("%s/files/user_%s", s.exPath, txtid)
		_, err := os.Stat(userDirectory)
		if os.IsNotExist(err) {
			errDir := os.MkdirAll(userDirectory, 0751)
			if errDir != nil {
				s.Respond(
					w,
					r,
					http.StatusInternalServerError,
					fmt.Errorf("could not create user directory (%s)", userDirectory),
				)
				return
			}
		}

		decoder := json.NewDecoder(r.Body)
		var t downloadAudioStruct
		err = decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		msg := &waProto.Message{AudioMessage: &waProto.AudioMessage{
			Url:           proto.String(t.Url),
			DirectPath:    proto.String(t.DirectPath),
			MediaKey:      t.MediaKey,
			Mimetype:      proto.String(t.Mimetype),
			FileEncSha256: t.FileEncSHA256,
			FileSha256:    t.FileSHA256,
			FileLength:    &t.FileLength,
		}}

		doc := msg.GetAudioMessage()

		if doc != nil {
			docdata, err = clientPointer[userid].Download(doc)
			if err != nil {
				log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to download audio")
				msg := fmt.Sprintf("Failed to download audio %v", err)
				s.Respond(w, r, http.StatusInternalServerError, errors.New(msg))
				return
			}
			mimetype = doc.GetMimetype()
		}

		dataURL := dataurl.New(docdata, mimetype)
		response := map[string]interface{}{"Mimetype": mimetype, "Data": dataURL.String()}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// React
func (s *server) React() http.HandlerFunc {

	type textStruct struct {
		Phone string
		Body  string
		Id    string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		msgid := ""
		var resp whatsmeow.SendResponse

		decoder := json.NewDecoder(r.Body)
		var t textStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing phone in payload"))
			return
		}

		if t.Body == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing body in payload"))
			return
		}

		recipient, ok := parseJID(t.Phone)
		if !ok {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse group jid"))
			return
		}

		if t.Id == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing id in payload"))
			return
		} else {
			msgid = t.Id
		}

		fromMe := false
		if strings.HasPrefix(msgid, "me:") {
			fromMe = true
			msgid = msgid[len("me:"):]
		}
		reaction := t.Body
		if reaction == "remove" {
			reaction = ""
		}

		msg := &waProto.Message{
			ReactionMessage: &waProto.ReactionMessage{
				Key: &waProto.MessageKey{
					RemoteJid: proto.String(recipient.String()),
					FromMe:    proto.Bool(fromMe),
					Id:        proto.String(msgid),
				},
				Text:              proto.String(reaction),
				GroupingKey:       proto.String(reaction),
				SenderTimestampMs: proto.Int64(time.Now().UnixMilli()),
			},
		}

		resp, err = clientPointer[userid].SendMessage(context.Background(), recipient, msg)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				fmt.Errorf("error sending message: %v", err),
			)
			return
		}

		log.Info().
			Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
			Str("id", msgid).
			Msg("Message sent")
		response := map[string]interface{}{
			"Details":   "Sent",
			"Timestamp": resp.Timestamp,
			"Id":        msgid,
		}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Mark messages as read
func (s *server) MarkRead() http.HandlerFunc {

	type markReadStruct struct {
		Id     []string
		Chat   types.JID
		Sender types.JID
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t markReadStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Chat.String() == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing chat in payload"))
			return
		}

		if len(t.Id) < 1 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing id in payload"))
			return
		}

		err = clientPointer[userid].MarkRead(t.Id, time.Now(), t.Chat, t.Sender)
		if err != nil {
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				errors.New("failure marking messages as read"),
			)
			return
		}

		response := map[string]interface{}{"Details": "Message(s) marked as read"}
		responseJson, err := json.Marshal(response)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// List groups
func (s *server) ListGroups() http.HandlerFunc {

	type GroupCollection struct {
		Groups []types.GroupInfo
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		resp, err := clientPointer[userid].GetJoinedGroups()

		if err != nil {
			msg := fmt.Sprintf("Failed to get group list: %v", err)
			log.Error().Msg(msg)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		gc := new(GroupCollection)
		for _, info := range resp {
			gc.Groups = append(gc.Groups, *info)
		}

		responseJson, err := json.Marshal(gc)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Get group info
func (s *server) GetGroupInfo() http.HandlerFunc {

	type getGroupInfoStruct struct {
		GroupJID string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t getGroupInfoStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		group, ok := parseJID(t.GroupJID)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse group jid"))
			return
		}

		resp, err := clientPointer[userid].GetGroupInfo(group)

		if err != nil {
			msg := fmt.Sprintf("Failed to get group info: %v", err)
			log.Error().Msg(msg)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		responseJson, err := json.Marshal(resp)

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Get group invite link
func (s *server) GetGroupInviteLink() http.HandlerFunc {

	type getGroupInfoStruct struct {
		GroupJID string
		Reset    bool
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t getGroupInfoStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		group, ok := parseJID(t.GroupJID)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse group jid"))
			return
		}

		resp, err := clientPointer[userid].GetGroupInviteLink(group, t.Reset)

		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to get group invite link")
			msg := fmt.Sprintf("Failed to get group invite link: %v", err)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		response := map[string]interface{}{"InviteLink": resp}
		responseJson, err := json.Marshal(response)

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Set group photo
func (s *server) SetGroupPhoto() http.HandlerFunc {

	type setGroupPhotoStruct struct {
		GroupJID string
		Image    string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t setGroupPhotoStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		group, ok := parseJID(t.GroupJID)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse group jid"))
			return
		}

		if t.Image == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing image in payload"))
			return
		}

		var filedata []byte

		if t.Image[0:13] == "data:image/jp" {
			dataURL, err := dataurl.DecodeString(t.Image)
			if err != nil {
				s.Respond(
					w,
					r,
					http.StatusBadRequest,
					errors.New("could not decode base64 encoded data from payload"),
				)
				return
			} else {
				filedata = dataURL.Data
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest, errors.New("image data should start with \"data:image/jpeg;base64,\""))
			return
		}

		picture_id, err := clientPointer[userid].SetGroupPhoto(group, filedata)

		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to set group photo")
			msg := fmt.Sprintf("Failed to set group photo: %v", err)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		response := map[string]interface{}{
			"Details":   "Group Photo set successfully",
			"PictureID": picture_id,
		}
		responseJson, err := json.Marshal(response)

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Set group name
func (s *server) SetGroupName() http.HandlerFunc {

	type setGroupNameStruct struct {
		GroupJID string
		Name     string
	}

	return func(w http.ResponseWriter, r *http.Request) {

		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t setGroupNameStruct
		err := decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		group, ok := parseJID(t.GroupJID)
		if !ok {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not parse group jid"))
			return
		}

		if t.Name == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing name in payload"))
			return
		}

		err = clientPointer[userid].SetGroupName(group, t.Name)

		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Failed to set group name")
			msg := fmt.Sprintf("Failed to set group name: %v", err)
			s.Respond(w, r, http.StatusInternalServerError, msg)
			return
		}

		response := map[string]interface{}{"Details": "Group Name set successfully"}
		responseJson, err := json.Marshal(response)

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		s.Respond(w, r, http.StatusOK, string(responseJson))
	}
}

// Create a User
func (s *server) CreateUser() http.HandlerFunc {
	type createUserStruct struct {
		Name  string
		Token string
	}

	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)
		name := ""

		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}

		log.Info().Msg("Looking for user information in DB")
		// Checks DB from matching user and store user values in context
		rows, err := s.db.Query("SELECT name FROM users WHERE id=? LIMIT 1", txtid)

		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error querying DB")
			s.Respond(w, r, http.StatusInternalServerError, errors.New("error querying db"))
			return
		}

		defer rows.Close()
		for rows.Next() {
			if err := rows.Scan(&name); err != nil {
				log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error scanning DB")
				s.Respond(w, r, http.StatusInternalServerError, errors.New("error scanning db"))
				return
			}
		}

		// if name not start with "super-" then return Unauthorized
		if !strings.HasPrefix(name, "super-") {
			s.Respond(w, r, http.StatusUnauthorized, errors.New("Unauthorized"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var t createUserStruct
		err = decoder.Decode(&t)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if t.Name == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing name in payload"))
			return
		}

		if t.Token == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("missing token in payload"))
			return
		}

		// Check if user already exists
		rows, err = s.db.Query("SELECT id FROM users WHERE token=? LIMIT 1", t.Token)
		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error querying DB")
			s.Respond(w, r, http.StatusInternalServerError, errors.New("error querying db"))
			return
		}

		defer rows.Close()
		for rows.Next() {
			s.Respond(w, r, http.StatusBadRequest, errors.New("user already exists"))
			return
		}

		// Create user
		stmt, err := s.db.Prepare("INSERT INTO users(name, token) VALUES(?, ?)")
		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error preparing DB statement")
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				errors.New("error preparing db statement"),
			)
			return
		}

		res, err := stmt.Exec(t.Name, t.Token)
		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error executing DB statement")
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				errors.New("error executing db statement"),
			)
			return
		}

		_, err = res.LastInsertId()
		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error getting last insert id")
			s.Respond(
				w,
				r,
				http.StatusInternalServerError,
				errors.New("error getting last insert id"),
			)
			return
		}

		// return response
		s.Respond(w, r, http.StatusOK, "User created successfully")
	}
}

// Writes JSON response to API clients
func (s *server) Respond(w http.ResponseWriter, r *http.Request, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	dataenvelope := map[string]interface{}{"code": status}
	if err, ok := data.(error); ok {
		dataenvelope["error"] = err.Error()
		dataenvelope["success"] = false
	} else {
		mydata := make(map[string]interface{})
		err = json.Unmarshal([]byte(data.(string)), &mydata)
		if err != nil {
			log.Error().Str("error", fmt.Sprintf("%v", err)).Msg("Error unmarshalling JSON")
		}
		dataenvelope["data"] = mydata
		dataenvelope["success"] = true
	}
	data = dataenvelope

	if err := json.NewEncoder(w).Encode(data); err != nil {
		panic("respond: " + err.Error())
	}
}

func validateMessageFields(phone string, stanzaid *string, participant *string) (types.JID, error) {

	recipient, ok := parseJID(phone)
	if !ok {
		return types.NewJID("", types.DefaultUserServer), errors.New("could not parse phone")
	}

	if stanzaid != nil {
		if participant == nil {
			return types.NewJID(
					"",
					types.DefaultUserServer,
				), errors.New(
					"missing participant in contextinfo",
				)
		}
	}

	if participant != nil {
		if stanzaid == nil {
			return types.NewJID(
					"",
					types.DefaultUserServer,
				), errors.New(
					"missing stanzaid in contextinfo",
				)
		}
	}

	return recipient, nil
}
