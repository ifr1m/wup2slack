package main

import (
	"bytes"
	fastshot "github.com/opus-domini/fast-shot"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleWhatsappInitiatedMessage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFbClient := NewMockFbClient(ctrl)
	mockSlackClient := NewMockSlackClient(ctrl)
	mockLinkedAccount := NewMockLinkedAccountStore(ctrl)

	mockFbClient.EXPECT().
		markWhatsappMessageAsRead("36825", "message_id_123").
		Return(fastshot.Response{}, nil)

	mockSlackClient.EXPECT().getSlackChannel(gomock.Any(), gomock.Any()).Return(slack.Channel{}, nil)
	mockSlackClient.EXPECT().postMessageAsUser(gomock.Any(), "message from whatsapp", "Bob").Return("", "", "", nil)
	mockLinkedAccount.EXPECT().lookupLinkedAccount(gomock.Any()).Return(&LinkedAccount{WhatsappBusinessPhoneNumberId: "36825", SlackUserId: "slack_123"}, nil)

	handler := handleWhatsappInitiatedMessage(mockFbClient, mockSlackClient, mockLinkedAccount)

	body := `{
    "object": "whatsapp_business_account",
    "entry": [
        {
            "id": "36825",
            "changes": [
                {
                    "value": {
                        "messaging_product": "whatsapp",
                        "metadata": {
                            "display_phone_number": "40742321321",
                            "phone_number_id": "36825"
                        },
                        "contacts": [
                            {
                                "profile": {
                                    "name": "Bob"
                                },
                                "wa_id": "40742123123"
                            }
                        ],
                        "messages": [
                            {
								"id": "message_id_123",
                                "from": "40742123123",
                                "text": {
                                    "body": "message from whatsapp"
                                },
                                "type": "text"
                            }
                        ]
                    },
                    "field": "messages"
                }
            ]
        }
    ]
}
`
	req, err := http.NewRequest("POST", "/whatsapp/webhook", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code, "Expected status code 200")
}
