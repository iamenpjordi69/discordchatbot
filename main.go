package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	dbClient   *mongo.Client
	channelCol *mongo.Collection
	myUserID   string
	groqKey    string
)

func healthCheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Bot is alive and well!")
	})
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func main() {
	godotenv.Load()
	
	// 1. START HEALTH CHECK IMMEDIATELY
	// This keeps Render happy even if everything else fails.
	go healthCheck()

	myUserID = os.Getenv("MY_USER_ID")
	groqKey = os.Getenv("GROQ_API_KEY")
	mongoURI := os.Getenv("MONGO_URI")
	botToken := os.Getenv("DISCORD_BOT_TOKEN")

	// 2. DIAGNOSTIC LOGGING (Safe for Render)
	log.Println("🚀 Starting Bot Setup...")
	if mongoURI == "" { log.Println("❌ MONGO_URI is MISSING") }
	if botToken == "" { log.Println("❌ DISCORD_BOT_TOKEN is MISSING") }
	if groqKey == "" { log.Println("❌ GROQ_API_KEY is MISSING") }

	if len(botToken) > 10 {
		// Check for common prefix issue
		if strings.HasPrefix(botToken, "Bot ") {
			log.Println("ℹ️ Note: Token already contains 'Bot ' prefix.")
		} else {
			botToken = "Bot " + botToken
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	if mongoURI != "" {
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
		if err != nil {
			log.Printf("⚠️ MongoDB Connection Error: %v (Bot will still try to start)", err)
		} else {
			channelCol = client.Database("discord_bot").Collection("permitted_channels")
			log.Println("✅ MongoDB connected (tentatively)")
		}
	}

	if botToken != "Bot " && botToken != "" {
		dg, err := discordgo.New(botToken)
		if err != nil {
			log.Printf("⚠️ Discord Initialization Error: %v", err)
		} else {
			dg.AddHandler(messageCreate)
			dg.AddHandler(handleInteraction)

			dg.Identify.Intents = discordgo.IntentsGuildMessages |
				discordgo.IntentMessageContent |
				discordgo.IntentsDirectMessages

			err = dg.Open()
			if err != nil {
				log.Printf("⚠️ Connection Error: %v", err)
			} else {
				log.Println("✅ Discord session opened!")
				defer dg.Close()
				
				// Sync commands
				time.Sleep(1 * time.Second)
				appID := dg.State.User.ID
				
				guildInstall := discordgo.ApplicationIntegrationType(0)
				userInstall := discordgo.ApplicationIntegrationType(1)
				guildContext := discordgo.InteractionContextType(0)
				dmContext := discordgo.InteractionContextType(1)
				privateContext := discordgo.InteractionContextType(2)

				commands := []*discordgo.ApplicationCommand{
					{
						Name: "ask",
						Description: "Ask the AI a question",
						IntegrationTypes: &[]discordgo.ApplicationIntegrationType{guildInstall, userInstall},
						Contexts: &[]discordgo.InteractionContextType{guildContext, dmContext, privateContext},
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString,
								Name: "question",
								Description: "Your question",
								Required: true,
							},
						},
					},
				}
				dg.ApplicationCommandBulkOverwrite(appID, "", commands)
				log.Println("✅ Slash commands synced.")
			}
		}
	} else {
		log.Println("❌ Skipping Discord initialization due to missing/invalid token.")
	}

	log.Println("🤖 Bot process is now waiting (Health Check is ACTIVE).")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	isPrivate := m.GuildID == ""
	isOwner := m.Author.ID == myUserID

	if !isPrivate && m.Content == "!activate" && isOwner {
		channelCol.UpdateOne(context.TODO(),
			map[string]string{"channel_id": m.ChannelID},
			map[string]interface{}{"$set": map[string]bool{"active": true}},
			options.Update().SetUpsert(true))
		s.ChannelMessageSend(m.ChannelID, "✅ AI Activated here.")
		return
	}

	if strings.HasPrefix(m.Content, "!ask ") || isMentioned(m, s.State.User.ID) {
		if !isPrivate && !isOwner {
			var res map[string]interface{}
			err := channelCol.FindOne(context.TODO(), map[string]string{"channel_id": m.ChannelID}).Decode(&res)
			if err != nil {
				return
			}
		}

		question := strings.TrimPrefix(m.Content, "!ask ")
		question = strings.TrimSpace(strings.TrimPrefix(question, fmt.Sprintf("<@%s>", s.State.User.ID)))
		s.ChannelTyping(m.ChannelID)
		answer := callGroq(question)
		s.ChannelMessageSendReply(m.ChannelID, answer, m.Reference())
	}
}

func handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()
	if data.Name != "ask" {
		return
	}

	var userID string
	if i.User != nil {
		userID = i.User.ID
	} else if i.Member != nil {
		userID = i.Member.User.ID
	}

	isPrivate := i.GuildID == ""
	isOwner := userID == myUserID

	if !isPrivate && !isOwner {
		var res map[string]interface{}
		err := channelCol.FindOne(context.TODO(), map[string]string{"channel_id": i.ChannelID}).Decode(&res)
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "❌ AI not activated in this server. Ask the owner to use `!activate`.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	question := data.Options[0].StringValue()
	answer := callGroq(question)

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &answer,
	})
}

func isMentioned(m *discordgo.MessageCreate, botID string) bool {
	for _, u := range m.Mentions {
		if u.ID == botID {
			return true
		}
	}
	return false
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

	client := &http.Client{Timeout: 25 * time.Second}
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