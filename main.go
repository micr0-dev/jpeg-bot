package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/mattn/go-mastodon"
	"golang.org/x/image/webp"
)

type Config struct {
	Server struct {
		MastodonServer string `toml:"mastodon_server"`
		ClientSecret   string `toml:"client_secret"`
		AccessToken    string `toml:"access_token"`
	} `toml:"server"`
}

var config Config
var ctx context.Context

func main() {
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading config.toml: %v", err)
	}

	ctx = context.Background()
	client := mastodon.NewClient(&mastodon.Config{
		Server:       config.Server.MastodonServer,
		ClientSecret: config.Server.ClientSecret,
		AccessToken:  config.Server.AccessToken,
	})

	ws := client.NewWSClient()
	events, err := ws.StreamingWSUser(ctx)
	if err != nil {
		log.Fatalf("Error connecting to streaming API: %v", err)
	}

	fmt.Println("jpeg-bot is live! Listening for events...")

	for event := range events {
		if notification, ok := event.(*mastodon.NotificationEvent); ok && notification.Notification.Type == "mention" {
			handleMention(client, notification.Notification)
		}
	}
}

func handleMention(client *mastodon.Client, notification *mastodon.Notification) {
	status := notification.Status
	images := collectImages(client, status)

	if len(images) == 0 {
		replyWithError(client, notification, "No images found to process.")
		return
	}

	for _, imageURL := range images {
		compressedJPEG, err := downloadAndCompressImage(imageURL)
		if err != nil {
			replyWithError(client, notification, fmt.Sprintf("Error compressing image: %v", err))
			continue
		}
		uploadMediaAndReply(client, compressedJPEG, notification, status.Visibility)
	}
}

func collectImages(client *mastodon.Client, status *mastodon.Status) []string {
	var images []string

	// Collect images from the current post
	for _, attachment := range status.MediaAttachments {
		if attachment.Type == "image" {
			images = append(images, attachment.URL)
		}
	}

	// If no images found, check if it's replying to another post
	if len(images) == 0 && status.InReplyToID != "" {
		originalStatusIDa := status.InReplyToID
		if originalStatusIDa == nil {
			return images
		}

		var originalStatusID mastodon.ID

		switch id := originalStatusIDa.(type) {
		case string:
			originalStatusID = mastodon.ID(id)
		case mastodon.ID:
			originalStatusID = id
		default:
			log.Printf("Unexpected type for InReplyToID: %T", originalStatusIDa)
		}

		originalStatus, err := client.GetStatus(ctx, originalStatusID)
		if err == nil {
			for _, attachment := range originalStatus.MediaAttachments {
				if attachment.Type == "image" {
					images = append(images, attachment.URL)
				}
			}
		}
	}

	return images
}

func downloadAndCompressImage(imageURL string) ([]byte, error) {
	resp, err := http.Get(imageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	imgData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	img, format, err := decodeImage(imgData)
	if err != nil {
		return nil, fmt.Errorf("error decoding image: %w", err)
	}

	log.Printf("Decoded image with format: %s", format)

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 5})
	if err != nil {
		return nil, fmt.Errorf("error encoding to jpeg: %w", err)
	}

	return buf.Bytes(), nil
}

func decodeImage(imgData []byte) (image.Image, string, error) {
	reader := bytes.NewReader(imgData)

	if strings.HasPrefix(fmt.Sprintf("%x", imgData[:8]), "89504e470d0a1a0a") {
		img, err := png.Decode(reader)
		if err == nil {
			return img, "png", nil
		}
		return nil, "", fmt.Errorf("PNG decoding failed: %w", err)
	}

	img, format, err := image.Decode(reader)
	if err == nil {
		return img, format, nil
	}

	reader.Seek(0, io.SeekStart)
	img, err = webp.Decode(reader)
	if err == nil {
		return img, "webp", nil
	}

	return nil, "", fmt.Errorf("unsupported image format")
}

func uploadMediaAndReply(client *mastodon.Client, compressedJPEG []byte, notification *mastodon.Notification, visibility string) {
	media, err := client.UploadMediaFromReader(ctx, bytes.NewReader(compressedJPEG))
	if err != nil {
		replyWithError(client, notification, fmt.Sprintf("Error uploading media: %v", err))
		return
	}

	if visibility == "public" {
		visibility = "unlisted"
	}

	reply := &mastodon.Toot{
		Status:      fmt.Sprintf("@%s Here's your compressed JPEG!", notification.Account.Acct),
		InReplyToID: notification.Status.ID,
		MediaIDs:    []mastodon.ID{media.ID},
		Visibility:  visibility,
	}

	_, err = client.PostStatus(ctx, reply)
	if err != nil {
		replyWithError(client, notification, fmt.Sprintf("Error posting reply: %v", err))
	}
}

func replyWithError(client *mastodon.Client, notification *mastodon.Notification, errorMsg string) {
	reply := &mastodon.Toot{
		Status:      fmt.Sprintf("@%s Oops! %s", notification.Account.Acct, errorMsg),
		InReplyToID: notification.Status.ID,
		Visibility:  notification.Status.Visibility,
	}

	_, err := client.PostStatus(ctx, reply)
	if err != nil {
		log.Printf("Error posting error reply: %v", err)
	}
}
