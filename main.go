package handler

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/appwrite/sdk-for-go/appwrite"
	"github.com/appwrite/sdk-for-go/client"
	"github.com/appwrite/sdk-for-go/databases"
	"github.com/appwrite/sdk-for-go/query"
	"github.com/bwmarrin/discordgo"
	"github.com/open-runtimes/types-for-go/v4/openruntimes"
)

var (
	appClient client.Client
	dbService *databases.Databases
	groqKey    string
	publicKey  string
	myUserID   string
	dbID       string
	once       sync.Once
)

func initialize() {
	once.Do(func() {
		groqKey = os.Getenv("GROQ_API_KEY")
		publicKey = os.Getenv("DISCORD_PUBLIC_KEY")
		myUserID = os.Getenv("MY_USER_ID")
		dbID = os.Getenv("APPWRITE_DATABASE_ID")
		if dbID == "" {
			dbID = "69d63761002f34dbe016" // Official Database ID (Oscar)
		}

		endpoint := os.Getenv("APPWRITE_FUNCTION_ENDPOINT")
		if endpoint == "" {
			endpoint = "https://cloud.appwrite.io/v1"
		}

		projectID := os.Getenv("APPWRITE_FUNCTION_PROJECT_ID")
		if projectID == "" {
			projectID = os.Getenv("PROJECT_ID")
		}

		apiKey := os.Getenv("APPWRITE_FUNCTION_API_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("API_KEY")
		}

		appClient = appwrite.NewClient(
			appwrite.WithEndpoint(endpoint),
			appwrite.WithProject(projectID),
			appwrite.WithKey(apiKey),
		)
		dbService = appwrite.NewDatabases(appClient)
	})
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
	initialize()

	headers := Context.Req.Headers
	signature := headers["x-signature-ed25519"]
	timestamp := headers["x-signature-timestamp"]
	body := Context.Req.BodyText()

	if !verifySignature(signature, timestamp, body, publicKey) {
		return Context.Res.Text("Invalid request signature", Context.Res.WithStatusCode(401))
	}

	var interaction discordgo.Interaction
	if err := json.Unmarshal([]byte(body), &interaction); err != nil {
		Context.Error("Failed to unmarshal interaction: " + err.Error())
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
				// 1. Check Global Ban & Authorisation
				userDocs, err := dbService.ListDocuments(dbID, "users", dbService.WithListDocumentsQueries([]string{query.Equal("user_id", userID)}))
				isBanned := false
				isAuthorised := false
				if err == nil && len(userDocs.Documents) > 0 {
					var data map[string]interface{}
					userDocs.Documents[0].Decode(&data)
					isBanned, _ = data["banned"].(bool)
					isAuthorised, _ = data["authorised"].(bool)
				} else if err != nil {
					Context.Error("Database error (users): " + err.Error())
				}

				if isBanned {
					return ephemeralResponse(Context, "❌ You have been banned from using this bot.")
				}

				// 2. Check Activation
				isPrivate := interaction.GuildID == ""
				if isPrivate {
					if !isAuthorised {
						return ephemeralResponse(Context, "❌ You are not authorised to use this bot as a personal app.")
					}
				} else {
					guildDocs, err := dbService.ListDocuments(dbID, "servers", dbService.WithListDocumentsQueries([]string{query.Equal("guild_id", interaction.GuildID)}))
					isActive := false
					if err == nil && len(guildDocs.Documents) > 0 {
						var data map[string]interface{}
						guildDocs.Documents[0].Decode(&data)
						isActive, _ = data["active"].(bool)
					} else if err != nil {
						Context.Error("Database error (servers): " + err.Error())
					}
					if !isActive {
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
			if interaction.GuildID == "" { return ephemeralResponse(Context, "❌ Use in a server.") }
			active := data.Name == "activate"
			
			upsertDocument(Context, dbID, "servers", interaction.GuildID, map[string]interface{}{
				"guild_id": interaction.GuildID,
				"active":   active,
			}, "guild_id")

			msg := "✅ Server Activated."
			if !active { msg = "❌ Server Deactivated." }
			return ephemeralResponse(Context, msg)

		case "authorise", "deauthorise":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			auth := data.Name == "authorise"
			
			upsertDocument(Context, dbID, "users", targetUser.ID, map[string]interface{}{
				"user_id":    targetUser.ID,
				"authorised": auth,
			}, "user_id")

			msg := fmt.Sprintf("✅ User %s authorised.", targetUser.Username)
			if !auth { msg = fmt.Sprintf("❌ User %s deauthorised.", targetUser.Username) }
			return ephemeralResponse(Context, msg)

		case "ban":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			upsertDocument(Context, dbID, "users", targetUser.ID, map[string]interface{}{
				"user_id": targetUser.ID,
				"banned":  true,
			}, "user_id")
			return ephemeralResponse(Context, fmt.Sprintf("⛔ User %s BANNED.", targetUser.Username))

		case "unban":
			if !isOwner { return ephemeralResponse(Context, "❌ Owner ONLY command.") }
			targetUser := data.Options[0].UserValue(nil)
			// For unban, we fetch the document ID to delete it
			docs, err := dbService.ListDocuments(dbID, "users", dbService.WithListDocumentsQueries([]string{query.Equal("user_id", targetUser.ID)}))
			if err == nil && len(docs.Documents) > 0 {
				_, err = dbService.DeleteDocument(dbID, "users", docs.Documents[0].Id)
				if err != nil {
					Context.Error("Failed to delete user doc: " + err.Error())
				}
			}
			return ephemeralResponse(Context, fmt.Sprintf("✅ User %s unbanned.", targetUser.Username))
		}
	}

	return Context.Res.Text("Unknown interaction", Context.Res.WithStatusCode(400))
}

func upsertDocument(Context openruntimes.Context, db, col, keyVal string, data map[string]interface{}, keyName string) {
	docs, err := dbService.ListDocuments(db, col, dbService.WithListDocumentsQueries([]string{query.Equal(keyName, keyVal)}))
	if err == nil && len(docs.Documents) > 0 {
		_, err = dbService.UpdateDocument(db, col, docs.Documents[0].Id, dbService.WithUpdateDocumentData(data))
		if err != nil {
			Context.Error("UpdateDocument Error ["+col+"]: " + err.Error())
		}
	} else {
		if err != nil && !strings.Contains(err.Error(), "404") {
			Context.Error("ListDocuments Error ["+col+"]: " + err.Error())
		}
		_, err = dbService.CreateDocument(db, col, "unique()", data)
		if err != nil {
			Context.Error("CreateDocument Error ["+col+"]: " + err.Error())
		}
	}
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
	req.Header.Set("Authorization", "Bearer "+os.Getenv("GROQ_API_KEY"))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return "⚠️ Groq Timeout" }
	defer resp.Body.Close()

	var res struct {
		Choices []struct { Message struct { Content string `json:"content"` } `json:"message"` } `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if len(res.Choices) > 0 { return res.Choices[0].Message.Content }
	return "⚠️ Groq Error"
}