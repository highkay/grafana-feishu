package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/gofiber/fiber/v2/middleware/logger"
	openai "github.com/sashabaranov/go-openai"
)

const systemPrompt = `你是经验丰富的SRE运维专家，请分析这个告警，并以飞书卡片能解析的Markdown格式返回，包含以下内容：

### 故障分析
[请在这里填写故障分析]

### 处置建议
[请在这里填写处置建议]

请使用中文回复。`

type Notification struct {
	Alerts []Alert `json:"alerts"`
}

type Alert struct {
	Status      string            `json:"status"`
	Annotations map[string]string `json:"annotations"`
	Labels      map[string]string `json:"labels"`
	StartsAt    string            `json:"startsAt"`
}

type FeishuCard struct {
	MsgType string            `json:"msg_type"`
	Card    FeishuCardContent `json:"card"`
}

type FeishuCardContent struct {
	Header   FeishuCardHeader       `json:"header"`
	Elements []FeishuCardDivElement `json:"elements"`
}

type FeishuCardHeader struct {
	Title    FeishuCardTextElement `json:"title"`
	Template string                `json:"template"`
}

type FeishuCardTextElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type FeishuCardDivElement struct {
	Tag  string                `json:"tag"`
	Text FeishuCardTextElement `json:"text"`
}

var defaultWebhookBase string = "https://open.feishu.cn/open-apis/bot/v2/hook"

func main() {
	var feishuWebhookBase string
	var defaultBotUUID string

	configuredWebhook := os.Getenv("FEISHU_WEBHOOK")
	if configuredWebhook != "" {
		re := regexp.MustCompile(`^(.*)/?([-a-z0-9]{36})?$`)
		matches := re.FindStringSubmatch(configuredWebhook)
		feishuWebhookBase = matches[1]
		defaultBotUUID = matches[2]
	} else {
		feishuWebhookBase = strings.TrimRight(os.Getenv("FEISHU_WEBHOOK_BASE"), "/")
		if feishuWebhookBase == "" {
			feishuWebhookBase = defaultWebhookBase
		}
		defaultBotUUID = os.Getenv("FEISHU_WEBHOOK_UUID")
	}
	if defaultBotUUID == "" {
		log.Println("defaultUUID not provided")
	}

	openaiToken := os.Getenv("OPENAI_API_KEY")
	openaiBaseURL := os.Getenv("OPENAI_BASE_URL")
	openaiModelName := os.Getenv("OPENAI_MODEL_NAME")
	if openaiModelName == "" {
		openaiModelName = openai.GPT3Dot5Turbo
	}

	app := fiber.New()
	app.Use(logger.New())

	webhookAuth := os.Getenv("WEBHOOK_AUTH")
	if webhookAuth != "" {
		log.Printf("Enabling basic auth")
		parts := strings.SplitN(webhookAuth, ":", 2)
		app.Use(basicauth.New(basicauth.Config{
			Users: map[string]string{
				parts[0]: parts[1],
			},
		}))
	}

	app.Post("/:botUUID?", func(c *fiber.Ctx) error {
		c.Accepts("application/json")
		notification := new(Notification)
		if err := c.BodyParser(notification); err != nil {
			return err
		}
		if len(notification.Alerts) == 0 {
			return nil
		}
		for _, alert := range notification.Alerts {
			title, ok := alert.Annotations["summary"]
			if !ok {
				title, ok = alert.Labels["alertname"]
				if !ok {
					title = "[No Title]"
				}
			}
			description, ok := alert.Annotations["description"]
			if !ok {
				description = "[No description]"
			}

			if openaiToken != "" {
				config := openai.DefaultConfig(openaiToken)
				if openaiBaseURL != "" {
					config.BaseURL = openaiBaseURL
				}
				client := openai.NewClientWithConfig(config)
				resp, err := client.CreateChatCompletion(
					context.Background(),
					openai.ChatCompletionRequest{
						Model: openaiModelName,
						Messages: []openai.ChatCompletionMessage{
							{
								Role:    openai.ChatMessageRoleSystem,
								Content: systemPrompt,
							},
							{
								Role:    openai.ChatMessageRoleUser,
								Content: description,
							},
						},
					},
				)
				if err == nil {
					description = resp.Choices[0].Message.Content
				}
			}

			color := "red"
			if alert.Status == "resolved" {
				color = "green"
			}
			feishuCard := &FeishuCard{
				MsgType: "interactive",
				Card: FeishuCardContent{
					Header: FeishuCardHeader{
						Title: FeishuCardTextElement{
							Tag:     "plain_text",
							Content: title,
						},
						Template: color,
					},
					Elements: []FeishuCardDivElement{
						{
							Tag: "div",
							Text: FeishuCardTextElement{
								Tag:     "lark_md",
								Content: description,
							},
						},
					},
				},
			}
			feishuJson, err := json.Marshal(feishuCard)
			if err != nil {
				return err
			}
			botUUID := c.Params("botUUID", defaultBotUUID)
			feishuWebhookURL := feishuWebhookBase + "/" + botUUID
			request, err := http.NewRequest("POST", feishuWebhookURL, bytes.NewBuffer(feishuJson))
			request.Header.Set("Content-Type", "application/json; charset=UTF-8")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			body, _ := ioutil.ReadAll(response.Body)
			log.Printf("Response body: %s", string(body))
		}
		return c.SendStatus(204)
	})

	app.Listen(":2387")
}