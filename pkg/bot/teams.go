// Copyright (c) 2020 InfraCloud Technologies
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/infracloudio/botkube/pkg/config"
	"github.com/infracloudio/botkube/pkg/events"
	"github.com/infracloudio/botkube/pkg/execute"
	"github.com/infracloudio/botkube/pkg/logging"
	"github.com/infracloudio/msbotbuilder-go/core"
	coreActivity "github.com/infracloudio/msbotbuilder-go/core/activity"
	"github.com/infracloudio/msbotbuilder-go/schema"
)

const (
	defaultMsgPath    = "/api/messages"
	defaultPort       = "3978"
	consentBufferSize = 100
	longRespNotice    = "Response is too long. Sending last few lines. Please send DM to BotKube to get complete response."
	convTypePersonal  = "personal"
	channelSetCmd     = "set default channel"
	maxMessageSize    = 15700
)

var _ Bot = (*Teams)(nil)

// Teams contains credentials to start Teams backend server
type Teams struct {
	AppID             string
	AppPassword       string
	MessagePath       string
	Port              string
	AllowKubectl      bool
	RestrictAccess    bool
	ClusterName       string
	NotifType         config.NotifType
	Adapter           core.Adapter
	ProcessedConsents chan processedConsent
	CleanupDone       chan bool

	ConversationRef *schema.ConversationReference
}

type processedConsent struct {
	ID              string
	conversationRef schema.ConversationReference
}

type ConsentContext struct {
	Command string
}

// NewTeamsBot returns Teams instance
func NewTeamsBot(c *config.Config) *Teams {
	logging.Logger.Infof("Config:: %+v", c.Communications.Teams)
	return &Teams{
		AppID:             c.Communications.Teams.AppID,
		AppPassword:       c.Communications.Teams.AppPassword,
		NotifType:         c.Communications.Teams.NotifType,
		MessagePath:       defaultMsgPath,
		Port:              defaultPort,
		AllowKubectl:      c.Settings.AllowKubectl,
		RestrictAccess:    c.Settings.RestrictAccess,
		ClusterName:       c.Settings.ClusterName,
		ProcessedConsents: make(chan processedConsent, consentBufferSize),
		CleanupDone:       make(chan bool),
	}
}

// Start MS Teams server to serve messages from Teams client
func (t *Teams) Start() {
	var err error
	setting := core.AdapterSetting{
		AppID:       t.AppID,
		AppPassword: t.AppPassword,
	}
	t.Adapter, err = core.NewBotAdapter(setting)
	if err != nil {
		logging.Logger.Errorf("Failed Start teams bot. %+v", err)
		return
	}
	// Start consent cleanup
	go t.cleanupConsents()
	http.HandleFunc(t.MessagePath, t.processActivity)
	logging.Logger.Infof("Started MS Teams server on port %s", defaultPort)
	logging.Logger.Errorf("Error in MS Teams server. %v", http.ListenAndServe(fmt.Sprintf(":%s", t.Port), nil))
	t.CleanupDone <- true
}

func (t *Teams) cleanupConsents() {
	for {
		select {
		case consent := <-t.ProcessedConsents:
			fmt.Printf("Deleting activity %s\n", consent.ID)
			if err := t.Adapter.DeleteActivity(context.Background(), consent.ID, consent.conversationRef); err != nil {
				logging.Logger.Errorf("Failed to delete activity. %s", err.Error())
			}
		case <-t.CleanupDone:
			return
		}
	}
}

func (t *Teams) processActivity(w http.ResponseWriter, req *http.Request) {
	ctx := context.Background()
	activity, err := t.Adapter.ParseRequest(ctx, req)
	if err != nil {
		logging.Logger.Errorf("Failed to parse Teams request. %s", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err = t.Adapter.ProcessActivity(ctx, activity, coreActivity.HandlerFuncs{
		OnMessageFunc: func(turn *coreActivity.TurnContext) (schema.Activity, error) {
			//actjson, _ := json.MarshalIndent(turn.Activity, "", "  ")
			//logging.Logger.Debugf("Received activity: %s", actjson)
			resp := t.processMessage(turn.Activity)
			if len(resp) >= maxMessageSize {
				if turn.Activity.Conversation.ConversationType == convTypePersonal {
					// send file upload request
					attachments := []schema.Attachment{
						{
							ContentType: "application/vnd.microsoft.teams.card.file.consent",
							Name:        "response.txt",
							Content: map[string]interface{}{
								"description": turn.Activity.Text,
								"sizeInBytes": len(resp),
								"acceptContext": map[string]interface{}{
									"command": activity.Text,
								},
							},
						},
					}
					return turn.SendActivity(coreActivity.MsgOptionAttachments(attachments))
				}
				resp = fmt.Sprintf("%s\n```\nCluster: %s\n%s", longRespNotice, t.ClusterName, resp[len(resp)-maxMessageSize:])
			}
			return turn.SendActivity(coreActivity.MsgOptionText(resp))
		},

		// handle invoke events
		// https://developer.microsoft.com/en-us/microsoft-teams/blogs/working-with-files-in-your-microsoft-teams-bot/
		OnInvokeFunc: func(turn *coreActivity.TurnContext) (schema.Activity, error) {
			t.pushProcessedConsent(turn.Activity.ReplyToID, coreActivity.GetCoversationReference(turn.Activity))
			if err != nil {
				return schema.Activity{}, fmt.Errorf("failed to read file: %s", err.Error())
			}
			if turn.Activity.Value["type"] != "fileUpload" {
				return schema.Activity{}, nil
			}
			if turn.Activity.Value["action"] != "accept" {
				return schema.Activity{}, nil
			}
			if turn.Activity.Value["context"] == nil {
				return schema.Activity{}, nil
			}

			// Parse upload info from invoke accept response
			uploadInfo := schema.UploadInfo{}
			infoJSON, err := json.Marshal(turn.Activity.Value["uploadInfo"])
			if err != nil {
				return schema.Activity{}, err
			}
			if err := json.Unmarshal(infoJSON, &uploadInfo); err != nil {
				return schema.Activity{}, err
			}

			// Parse context
			consentCtx := ConsentContext{}
			ctxJSON, err := json.Marshal(turn.Activity.Value["context"])
			if err != nil {
				return schema.Activity{}, err
			}
			if err := json.Unmarshal(ctxJSON, &consentCtx); err != nil {
				return schema.Activity{}, err
			}

			msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(consentCtx.Command), "<at>BotKube</at>"))
			e := execute.NewDefaultExecutor(msg, t.AllowKubectl, t.RestrictAccess, t.ClusterName, true)
			out := e.Execute()

			aj, _ := json.MarshalIndent(turn.Activity, "", "  ")
			fmt.Printf("Incoming Activity:: \n%s\n", aj)

			// upload file
			err = t.putRequest(uploadInfo.UploadURL, []byte(out))
			if err != nil {
				return schema.Activity{}, fmt.Errorf("failed to upload file: %s", err.Error())
			}

			// notify user about uploaded file
			fileAttach := []schema.Attachment{
				{
					ContentType: "application/vnd.microsoft.teams.card.file.info",
					ContentURL:  uploadInfo.ContentURL,
					Name:        uploadInfo.Name,
					Content: map[string]interface{}{
						"uniqueId": uploadInfo.UniqueID,
						"fileType": uploadInfo.FileType,
					},
				},
			}

			return turn.SendActivity(coreActivity.MsgOptionAttachments(fileAttach))
		},
	})
	if err != nil {
		logging.Logger.Errorf("Failed to process request. %s", err.Error())
	}
}

func (t *Teams) processMessage(activity schema.Activity) string {
	// Trim @BotKube prefix
	msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(activity.Text), "<at>BotKube</at>"))

	// Parse "set default channel" command and set conversation reference
	if msg == channelSetCmd {
		ref := coreActivity.GetCoversationReference(activity)
		t.ConversationRef = &ref
		// Remove messageID from the ChannelID
		if ID, ok := activity.ChannelData["teamsChannelId"]; ok {
			t.ConversationRef.ChannelID = ID.(string)
			t.ConversationRef.Conversation.ID = ID.(string)
		}
		return "Okay. I'll send notifications to this channel"
	}

	// Multicluster is not supported for Teams
	e := execute.NewDefaultExecutor(msg, t.AllowKubectl, t.RestrictAccess, t.ClusterName, true)
	out := e.Execute()
	return fmt.Sprintf("```%s```", out)
}

func (t *Teams) pushProcessedConsent(ID string, ref schema.ConversationReference) {
	select {
	case t.ProcessedConsents <- processedConsent{ID: ID, conversationRef: ref}:
		break
	default:
		// Remove older ID if buffer is full
		<-t.ProcessedConsents
		t.ProcessedConsents <- processedConsent{ID: ID, conversationRef: ref}
	}
}

func (t *Teams) putRequest(u string, data []byte) error {
	client := &http.Client{}
	dec, err := url.QueryUnescape(u)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, dec, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	size := fmt.Sprintf("%d", len(data))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", size)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("failed to upload file with status %d", resp.StatusCode)
	}
	return nil
}

func (t *Teams) SendEvent(event events.Event) error {
	card := formatTeamsMessage(event, t.NotifType)
	if err := t.sendProactiveMessage(card); err != nil {
		logging.Logger.Errorf("Failed to send notification. %s", err.Error())
	}
	logging.Logger.Debugf("Event successfully sent to MS Teams >> %+v", event)
	return nil
}

// SendMessage sends message to MsTeams
func (t *Teams) SendMessage(msg string) error {
	if t.ConversationRef == nil {
		logging.Logger.Infof("Skipping SendMessage since conversation ref not set")
		return nil
	}
	err := t.Adapter.ProactiveMessage(context.TODO(), *t.ConversationRef, coreActivity.HandlerFuncs{
		OnMessageFunc: func(turn *coreActivity.TurnContext) (schema.Activity, error) {
			return turn.SendActivity(coreActivity.MsgOptionText(msg))
		},
	})
	if err != nil {
		return err
	}
	logging.Logger.Debug("Message successfully sent to MS Teams")
	return nil
}

func (t *Teams) sendProactiveMessage(card map[string]interface{}) error {
	if t.ConversationRef == nil {
		logging.Logger.Infof("Skipping SendMessage since conversation ref not set")
		return nil
	}
	err := t.Adapter.ProactiveMessage(context.TODO(), *t.ConversationRef, coreActivity.HandlerFuncs{
		OnMessageFunc: func(turn *coreActivity.TurnContext) (schema.Activity, error) {
			attachments := []schema.Attachment{
				{
					ContentType: "application/vnd.microsoft.card.adaptive",
					Content:     card,
				},
			}
			return turn.SendActivity(coreActivity.MsgOptionAttachments(attachments))
		},
	})
	return err
}
