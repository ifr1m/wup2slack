package main

//go:generate mockgen -source=${GOFILE} -destination=mock_main_test.go -package=$GOPACKAGE -self_package=github.com/wup2slack

import (
	"bytes"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	fastshot "github.com/opus-domini/fast-shot"
	"github.com/slack-go/slack"
	"github.com/valyala/fastjson"
	"github.com/ztrue/tracerr"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

type Wup2SlackError struct {
	ErrorMessage string
	Cause        error
}

func (err *Wup2SlackError) Error() string {
	return fmt.Sprintf("%s. Cause: %s", err.ErrorMessage, err.Cause)
}
func (err *Wup2SlackError) Unwrap() error {
	return err.Cause
}

func NewWup2SlackError(errorMessage string, cause error) *Wup2SlackError {
	return &Wup2SlackError{
		ErrorMessage: errorMessage,
		Cause:        cause,
	}
}

type FbClient interface {
	fastshot.ClientHttpMethods
	sendWhatsappMessage(businessPhoneNumberId string, to string, message string, replyToMessage string) (fastshot.Response, error)
	markWhatsappMessageAsRead(businessPhoneNumberId string, messageId string) (fastshot.Response, error)
}

type FbClientS struct {
	fastshot.ClientHttpMethods
}

func (t *FbClientS) sendWhatsappMessage(businessPhoneNumberId string, to string, message string, replyToMessage string) (fastshot.Response, error) {
	var messageToSend *WhatsappMessageSendType
	if replyToMessage == "" {
		messageToSend = NewWhatsappMessageSendType(to, Text{Body: message}, nil)

	} else {
		messageToSend = NewWhatsappMessageSendType(to, Text{Body: message}, &Context{MessageID: replyToMessage})
	}
	a, _ := json.Marshal(messageToSend)
	fmt.Println(string(a))
	r, e := t.POST(fmt.Sprintf("/%v/messages", businessPhoneNumberId)).
		Header().AddContentType("application/json").
		Body().AsJSON(messageToSend).Send()

	return t.handleErrors(e, r)
}

func (t *FbClientS) handleErrors(e error, r fastshot.Response) (fastshot.Response, error) {
	if e != nil {
		return r, tracerr.Wrap(e)
	}
	if r.IsError() {
		errorBody, _ := io.ReadAll(r.RawBody())
		return r, tracerr.Errorf("Got status %s: %s", r.Status(), string(errorBody))
	}
	return r, nil
}

func (t *FbClientS) markWhatsappMessageAsRead(businessPhoneNumberId string, messageId string) (fastshot.Response, error) {
	messageReadBody := WhatsappMessageRead{MessagingProduct: "whatsapp", Status: "read", MessageId: messageId}
	r, e := t.POST(fmt.Sprintf("/%v/messages", businessPhoneNumberId)).
		Header().AddContentType("application/json").
		Body().AsJSON(messageReadBody).Send()
	return t.handleErrors(e, r)
}

type SlackClient interface {
	initializeNewChannel(initiatorDetails WhatsappInitiatorDetails, linkedAccount *LinkedAccount) (*slack.Channel, error)
	getExistingChannel(slackUserId string, whatsAppPhoneNumber string) (slack.Channel, error, bool)
	getSlackChannel(linkedAccount *LinkedAccount, initiatorDetails WhatsappInitiatorDetails) (slack.Channel, error)
	postMessageAsUser(channelId string, message string, asUserName string) (channel string, timestamp string, responseText string, err error)
	GetConversationInfo(input *slack.GetConversationInfoInput) (*slack.Channel, error)
}

type SlackClientS struct {
	slack.Client
}

func (t *SlackClientS) initializeNewChannel(initiatorDetails WhatsappInitiatorDetails, linkedAccount *LinkedAccount) (*slack.Channel, error) {
	createdChannel, err := t.CreateConversation(slack.CreateConversationParams{ChannelName: initiatorDetails.getDefaultSlackChannelName()})
	if err != nil {
		return nil, tracerr.Wrap(NewWup2SlackError("error creating channel", err))
	}
	_, err = t.InviteUsersToConversation(createdChannel.ID, linkedAccount.SlackUserId)
	if err != nil {
		return nil, tracerr.Wrap(NewWup2SlackError("error inviting user to channel", err))
	}
	data, err := json.Marshal(initiatorDetails)
	if err != nil {
		return nil, tracerr.Wrap(NewWup2SlackError("error marshalling initiator details", err))
	}
	updatedChannel, err := t.SetPurposeOfConversation(createdChannel.ID, string(data))
	if err != nil {
		return nil, tracerr.Wrap(NewWup2SlackError("error setting channel purpose/description", err))
	}
	return updatedChannel, nil
}

func (t *SlackClientS) getExistingChannel(slackUserId string, whatsAppPhoneNumber string) (slack.Channel, error, bool) {
	nextCursor := ""
	for goToNextPage := true; goToNextPage; goToNextPage = nextCursor != "" {
		existingChannels, cursor, err := t.GetConversationsForUser(&slack.GetConversationsForUserParameters{UserID: slackUserId, ExcludeArchived: true, Cursor: nextCursor})
		nextCursor = cursor
		if err != nil {
			return slack.Channel{}, tracerr.Wrap(err), false
		}
		for _, existingChannel := range existingChannels {
			if strings.Contains(existingChannel.Purpose.Value, whatsAppPhoneNumber) {
				return existingChannel, nil, true
			}
		}
	}
	return slack.Channel{}, nil, false
}

func (t *SlackClientS) getSlackChannel(linkedAccount *LinkedAccount, initiatorDetails WhatsappInitiatorDetails) (slack.Channel, error) {
	existingChannel, err, channelFound := t.getExistingChannel(linkedAccount.SlackUserId, initiatorDetails.WhatsappPhoneNumber)
	if err != nil {
		return slack.Channel{}, tracerr.Wrap(NewWup2SlackError("error getting existing channel", err))
	}

	if !channelFound {
		newChannel, err := t.initializeNewChannel(initiatorDetails, linkedAccount)
		if err != nil {
			return slack.Channel{}, tracerr.Wrap(err)
		}
		existingChannel = *newChannel
	}
	return existingChannel, nil
}

func (t *SlackClientS) postMessageAsUser(channelId string, message string, asUserName string) (channel string, timestamp string, responseText string, err error) {
	channel, timestamp, responseText, e := t.SendMessage(channelId, slack.MsgOptionPost(), slack.MsgOptionText(message, false), slack.MsgOptionUsername(asUserName))
	err = tracerr.Wrap(e)
	return
}

type LinkedAccount struct {
	WhatsappBusinessPhoneNumberId string `json:"whatsapp_business_phone_number_id"`
	SlackUserId                   string `json:"slack_user_id"`
}

type LinkedAccountStore interface {
	lookupLinkedAccount(id string) (*LinkedAccount, error)
}

type LinkedAccountFileStore struct {
	linkedAccounts []LinkedAccount
}

func NewLinkedAccountFileStore() (*LinkedAccountFileStore, error) {
	linkedAccountsFile := os.Getenv("LINKED_ACCOUNTS_FILE_STORE")
	b, err := os.ReadFile(linkedAccountsFile)
	if err != nil {
		return nil, tracerr.Wrap(err)
	}
	var linkedAccounts []LinkedAccount
	err = json.Unmarshal(b, &linkedAccounts)
	if err != nil {
		return nil, tracerr.Wrap(err)
	}

	return &LinkedAccountFileStore{linkedAccounts: linkedAccounts}, nil
}

func (t *LinkedAccountFileStore) lookupLinkedAccount(id string) (*LinkedAccount, error) {
	for _, la := range t.linkedAccounts {
		if la.WhatsappBusinessPhoneNumberId == id || la.SlackUserId == id {
			return &la, nil
		}
	}
	return nil, tracerr.New("linkedAccount not found")
}

type Text struct {
	Body string `json:"body"`
}
type Context struct {
	MessageID string `json:"message_id"`
}

type WhatsappMessageSendType struct {
	MessagingProduct string   `json:"messaging_product"`
	To               string   `json:"to"`
	Type             string   `json:"type"`
	Text             Text     `json:"text"`
	Context          *Context `json:"context"`
}

func NewWhatsappMessageSendType(to string, text Text, context *Context) *WhatsappMessageSendType {
	return &WhatsappMessageSendType{MessagingProduct: "whatsapp", To: to, Type: "text", Text: text, Context: context}
}

type WhatsappMessageRead struct {
	MessagingProduct string `json:"messaging_product"`
	Status           string `json:"status"`
	MessageId        string `json:"message_id"`
}

type WhatsappInitiatorDetails struct {
	Name                string `json:"name"`
	WhatsappPhoneNumber string `json:"whatsapp_phone_number"`
}

func (t WhatsappInitiatorDetails) getDefaultSlackChannelName() string {
	return fmt.Sprintf("%s-%s", strings.ToLower(t.Name), strings.ToLower(t.WhatsappPhoneNumber))
}

func main() {
	ctx := context.Background()
	smClient, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Fatalf("Failed to configure secrets manager client %+v\n", err)
		return
	}
	graphApiToken, err := smClient.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: os.Getenv("GRAPH_API_TOKEN_SECRET_NAME"),
	})
	if err != nil {
		log.Fatalf("Failed to access secret GRAPH_API_TOKEN: %v", err)
		return
	}
	slackApiToken, err := smClient.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: os.Getenv("SLACK_BOT_TOKEN_SECRET_NAME"),
	})
	if err != nil {
		log.Fatalf("Failed to access secret SLACK_BOT_TOKEN: %v", err)
		return
	}

	whatsappWebhookVerifyToken, err := smClient.AccessSecretVersion(context.Background(), &secretmanagerpb.AccessSecretVersionRequest{
		Name: os.Getenv("WUP_WEBHOOK_VERIFY_TOKEN_SECRET_NAME"),
	})
	if err != nil {
		log.Fatalf("Failed to access secret WUP_WEBHOOK_VERIFY_TOKEN: %v", err)
		return
	}

	var fbClient = &FbClientS{fastshot.NewClient("https://graph.facebook.com/v20.0").
		Auth().BearerToken(string(graphApiToken.Payload.Data)).
		Build()}
	slackClient := &SlackClientS{*slack.New(string(slackApiToken.Payload.Data))}

	linkedAccountStore, err := NewLinkedAccountFileStore()
	if err != nil {
		log.Fatalf("Failed to create linked account store %+v\n", err)
	}
	r := mux.NewRouter()
	r.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("Nothing to see here."))
	})
	r.HandleFunc("/whatsapp/webhook", resolveWhatsappVerificationChallenge(string(whatsappWebhookVerifyToken.Payload.Data))).Queries().Methods("GET")
	r.HandleFunc("/whatsapp/webhook", handleWhatsappInitiatedMessage(fbClient, slackClient, linkedAccountStore))
	r.HandleFunc("/slack/webhook", handleSlackInitiatedMessage(fbClient, slackClient, linkedAccountStore))
	log.Println("Listening on port: 8080")
	log.Fatal(http.ListenAndServe(":8080", r))

}

func resolveWhatsappVerificationChallenge(whatsappWebhookVerifyToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Rceived url rquest for whatsapp verification :%s\n", r.URL.String())
		mode := r.URL.Query()["hub.mode"][0]
		token := r.URL.Query()["hub.verify_token"][0]
		challenge := r.URL.Query()["hub.challenge"][0]

		if mode == "subscribe" && token == whatsappWebhookVerifyToken {
			w.WriteHeader(200)
			_, err := w.Write([]byte(challenge))
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				log.Printf("Error sending the chaleenge %+v\n", err)
				return
			}
			log.Println("Webhook verified successfully!")
		} else {
			w.WriteHeader(http.StatusForbidden)
			log.Println("Webhook cannot be verified!")
		}
	}
}

func handleWhatsappInitiatedMessage(fbClient FbClient, slackClient SlackClient, store LinkedAccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bodyValue, err := getBody(r)
		if err != nil {
			handleBadRequest(w, err, "Error reading body for whatsapp initiated message")
			return
		}
		newMessage := bodyValue.Get("entry", "0", "changes", "0", "value", "messages", "0")

		if bytes.Equal(newMessage.GetStringBytes("type"), []byte("text")) {
			businessPhoneNumberId := string(bodyValue.GetStringBytes("entry", "0", "changes", "0", "value", "metadata", "phone_number_id"))
			linkedAccount, err := store.lookupLinkedAccount(businessPhoneNumberId)
			if err != nil {
				handleInternalError(w, err, "Error looking up linked account for whatsapp initiated message")
				return
			}
			initiatorDetails, err := getInitiatorDetails(bodyValue, newMessage)
			if err != nil {
				handleBadRequest(w, err, "Error getting whatsapp initiator details")
				return
			}

			channel, err := slackClient.getSlackChannel(linkedAccount, initiatorDetails)
			if err != nil {
				handleInternalError(w, err, "Error getting slack channel")
				return
			}

			//forward whatsapp message to slack.
			_, _, _, err = slackClient.postMessageAsUser(channel.ID, string(newMessage.GetStringBytes("text", "body")), initiatorDetails.Name)
			if err != nil {
				handleInternalError(w, err, "Error forwarding whatsapp message to slack")
				return
			}

			//mark message as read in whatsapp
			_, err = fbClient.markWhatsappMessageAsRead(businessPhoneNumberId, string(newMessage.GetStringBytes("id")))
			if err != nil {
				handleInternalError(w, err, "Error marking message as read in whatsapp")
				return
			}
			w.WriteHeader(http.StatusOK)
		}

	}
}

func handleSlackInitiatedMessage(fbClient FbClient, slackApi SlackClient, store LinkedAccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bodyValue, err := getBody(r)
		if err != nil {
			handleBadRequest(w, err, "Error reading body for slack initiated message")
			return
		}
		if isWebhookUrlVerificationRequest(bodyValue) {
			resolveSlackVerificationChallenge(w, bodyValue)
			return
		} else if isRegularMessageEvent(bodyValue) {

			channel, err := slackApi.GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: string(bodyValue.GetStringBytes("event", "channel"))})
			if err != nil {
				handleInternalError(w, err, "Error getting slack channel")
				return
			}
			whatsappDetails := WhatsappInitiatorDetails{}
			err = json.Unmarshal([]byte(channel.Purpose.Value), &whatsappDetails)
			if err != nil {
				handleInternalError(w, err, "Error getting whatsapp details from channel purpose(description)")
				return
			}
			linkedAccount, err := store.lookupLinkedAccount(string(bodyValue.GetStringBytes("event", "user")))
			if err != nil {
				handleInternalError(w, err, "Error looking up linked account for slack initiated message")
				return
			}
			_, err = fbClient.sendWhatsappMessage(linkedAccount.WhatsappBusinessPhoneNumberId, whatsappDetails.WhatsappPhoneNumber, string(bodyValue.GetStringBytes("event", "text")), "")
			if err != nil {
				handleInternalError(w, err, "Error forwarding slack message to whatsapp")
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}

}

func isRegularMessageEvent(bodyValue *fastjson.Value) bool {
	return bytes.Equal(bodyValue.GetStringBytes("type"), []byte("event_callback")) && bytes.Equal(bodyValue.GetStringBytes("event", "type"), []byte("message")) && !bodyValue.Exists("event", "subtype")
}

func resolveSlackVerificationChallenge(w http.ResponseWriter, bodyValue *fastjson.Value) {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(bodyValue.GetStringBytes("challenge"))
	if err != nil {
		log.Printf("Error writing response when verifying the the slack api integration: %+v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func isWebhookUrlVerificationRequest(bodyValue *fastjson.Value) bool {
	return bytes.Equal(bodyValue.GetStringBytes("type"), []byte("url_verification"))
}

func getInitiatorDetails(bodyValue *fastjson.Value, newMessage *fastjson.Value) (WhatsappInitiatorDetails, error) {
	newMessageContacts := bodyValue.GetArray("entry", "0", "changes", "0", "value", "contacts")
	for _, contact := range newMessageContacts {
		if bytes.Equal(contact.GetStringBytes("wa_id"), newMessage.GetStringBytes("from")) {
			whatsappUserName := string(contact.GetStringBytes("profile", "name"))
			whatsappPhoneNumber := string(contact.GetStringBytes("wa_id"))
			return WhatsappInitiatorDetails{
				Name:                whatsappUserName,
				WhatsappPhoneNumber: whatsappPhoneNumber,
			}, nil
		}
	}
	return WhatsappInitiatorDetails{}, tracerr.New("could not find associated contact for the initiated whatsapp message. something changed in the whatsapp callback message")
}

func getBody(r *http.Request) (*fastjson.Value, error) {
	var s fastjson.Scanner
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, tracerr.Wrap(err)
	}
	s.InitBytes(bodyBytes)
	s.Next()
	bodyValue := s.Value()
	return bodyValue, nil
}

func handleInternalError(w http.ResponseWriter, err error, contextMessage string) {
	w.WriteHeader(http.StatusInternalServerError)
	log.Println(contextMessage)
	tracerr.PrintSource(err)
}
func handleBadRequest(w http.ResponseWriter, err error, contextMessage string) {
	w.WriteHeader(http.StatusBadRequest)
	log.Println(contextMessage)
	tracerr.PrintSource(err)
}
