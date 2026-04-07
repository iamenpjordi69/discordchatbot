package handler

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/open-runtimes/types-for-go/v4/openruntimes"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	dbClient   *mongo.Client
	channelCol *mongo.Collection
	userCol    *mongo.Collection
	groqKey    string
	publicKey  string
	myUserID   string
	once       sync.Once
)

func initialize() error {
	var err error
	once.Do(func() {
		groqKey = os.Getenv("GROQ_API_KEY")
		publicKey = os.Getenv("DISCORD_PUBLIC_KEY")
		myUserID = os.Getenv("MY_USER_ID")
		mongoURI := os.Getenv("MONGO_URI")

		if mongoURI != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dbClient, err = mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
			if err == nil {
				db := dbClient.Database("discord_bot")
				channelCol = db.Collection("permitted_channels")
				userCol = db.Collection("users")
			}
		}
	})
	return err
}

func verifySignature(signature, timestamp, body, pubKeyHex string) bool {
	pubKey, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		return false
	}

	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}

	var msg bytes.Buffer
	msg.WriteString(timestamp)
	msg.WriteString(body)

	return ed25519.Verify(pubKey, msg.Bytes(), sig)
}

func Main(Context openruntimes.Context) openruntimes.Response {
	if err := initialize(); err != nil {
		Context.Error("Initialization failed: " + err.Error())
		return Context.Res.Json(map[string]string{"error": "Initialization failed"}, Context.Res.WithStatusCode(500))
	}

	headers := Context.Req.Headers
	signature := headers["x-signature-ed25519"]
	timestamp := headers["x-signature-timestamp"]
	body := Context.Req.BodyText()

	if !verifySignature(signature, timestamp, body, publicKey) {
		return Context.Res.Text("Invalid request signature", Context.Res.WithStatusCode(401))
	}

	var interaction discordgo.Interaction
	if err := json.Unmarshal([]byte(body), &interaction); err != nil {
		return Context.Res.Text("Invalid payload", Context.Res.WithStatusCode(400))
	}

	switch interaction.Type {
	case discordgo.InteractionPing:
		return Context.Res.Json(map[string]interface{}{"type": 1}, Context.Res.WithStatusCode(200))

	case discordgo.InteractionApplicationCommand:
		data := interaction.ApplicationCommandData()
		
		var userID string
		if interaction.Member != nil {
			userID = interaction.Member.User.ID
		} else if interaction.User != nil {
			userID = interaction.User.ID
		}
		isOwner := userID == myUserID

		switch data.Name {
		case "ask":
			question := data.Options[0].StringValue()

			if !isOwner {
				// 1. Check Global Ban
				var userRecord map[string]interface{}
				userCol.FindOne(context.TODO(), map[string]string{"user_id": userID}).Decode(&userRecord)
				if banned, _ := userRecord["banned"].(bool); banned {
					return ephemeralResponse(Context, "❌ You have been banned from using this bot.")
				}

				// 2. Check Activation
				isPrivate := interaction.GuildID == ""
				if isPrivate {
					// User App Context (DM/Private)
					if authorised, _ := userRecord["authorised"].(bool); !authorised {
						return ephemeralResponse(Context, "❌ You are not authorised to use this bot as a personal app. Contact the owner.")
					}
				} else {
					// Server Context
					var guildRecord map[string]interface{}
					err := channelCol.FindOne(context.TODO(), map[string]string{"guild_id": interaction.GuildID}).Decode(&guildRecord)
					if err != nil || (!guildRecord["active"].(bool)) {
						return ephemeralResponse(Context, "❌ This server is not activated. Ask the owner to run `/activate`.")
					}
				}
			}

			answer := callGroq(question)
			return Context.Res.Json(map[string]interface{}{
				"type": 4,
				"data": map[string]interface{}{
					"content": answer,
				},
			}, Context.Res.WithStatusCode(200))

		case "activate", "deactivate":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			if interaction.GuildID == "" { return ephemeralResponse(Context, "❌ This command must be used in a server.") }
			active := data.Name == "activate"
			channelCol.UpdateOne(context.TODO(),
				map[string]string{"guild_id": interaction.GuildID},
				map[string]interface{}{"$set": map[string]interface{}{"active": active}},
				options.Update().SetUpsert(true))
			msg := "✅ Server Activated."
			if !active { msg = "❌ Server Deactivated." }
			return ephemeralResponse(Context, msg)

		case "authorise", "deauthorise":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			authorised := data.Name == "authorise"
			userCol.UpdateOne(context.TODO(),
				map[string]string{"user_id": targetUser.ID},
				map[string]interface{}{"$set": map[string]interface{}{"authorised": authorised}},
				options.Update().SetUpsert(true))
			msg := fmt.Sprintf("✅ User %s authorised for personal use.", targetUser.Username)
			if !authorised { msg = fmt.Sprintf("❌ User %s deauthorised.", targetUser.Username) }
			return ephemeralResponse(Context, msg)

		case "ban":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			userCol.UpdateOne(context.TODO(),
				map[string]string{"user_id": targetUser.ID},
				map[string]interface{}{"$set": map[string]interface{}{"banned": true}},
				options.Update().SetUpsert(true))
			return ephemeralResponse(Context, fmt.Sprintf("⛔ User %s has been GLOBALLY BANNED.", targetUser.Username))

		case "unban":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			userCol.DeleteOne(context.TODO(), map[string]string{"user_id": targetUser.ID})
			return ephemeralResponse(Context, fmt.Sprintf("✅ User %s has been unbanned and reset.", targetUser.Username))
		}
	}

	return Context.Res.Text("Unknown interaction", Context.Res.WithStatusCode(400))
}

func ephemeralResponse(Context openruntimes.Context, msg string) openruntimes.Response {
	return Context.Res.Json(map[string]interface{}{
		"type": 4,
		"data": map[string]interface{}{
			"content": msg,
			"flags":   64,
		},
	}, Context.Res.WithStatusCode(200))
}

func callGroq(prompt string) string {
	url := "https://api.groq.com/openai/v1/chat/completions"
	payload := map[string]interface{}{
		"model": "llama-3.3-70b-versatile",
		"messages": []map[string]string{
			{"role": "system", "content": "Concise AI. Under 1900 chars."},
			{"role": "user", "content": prompt},
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+groqKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "⚠️ Groq API Timeout"
	}
	defer resp.Body.Close()

	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if len(res.Choices) > 0 {
		return res.Choices[0].Message.Content
	}
	return "⚠️ Groq returned an error."
}