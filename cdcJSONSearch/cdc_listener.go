package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/redis/go-redis/v9"
)

// Declare a global context background for Redis commands
var ctx = context.Background()

type PayloadItem struct {
	After struct {
		City            string `json:"city"`
		CreationTime    string `json:"creation_time"`
		CurrentLocation string `json:"current_location"`
		Ext             struct {
			Brand string `json:"brand"`
			Color string `json:"color"`
		} `json:"ext"`
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
		Status  string `json:"status"`
		Type    string `json:"type"`
	} `json:"after"`
	Key     []string `json:"key"`
	Topic   string   `json:"topic"`
	Updated string   `json:"updated"`
}

type RequestBody struct {
	Payload []PayloadItem `json:"payload"`
	Length  int           `json:"length"`
}

const (
	CertPath string = "cert.pem"
	KeyPath  string = "key.pem"
)

func main() {
	// 1. Define command-line flags
	appPort := flag.Int("port", 3000, "Port for this application to run on")
	redisHost := flag.String("host", "localhost", "Redis/DragonflyDB server hostname")
	redisPort := flag.Int("redis-port", 6379, "Redis/DragonflyDB server port")
	useTLS := flag.Bool("use-tls", false, "Enable TLS connection to Redis/DragonflyDB")
	skipVerify := flag.Bool("skip-verify", true, "Skip TLS verification for Redis (set false for production public CAs)")
	username := flag.String("username", "default", "Username for Redis authentication (optional)")
	password := flag.String("password", "", "Password for Redis authentication (optional)")
	// 2. Parse the flags
	flag.Parse()

	log.Printf("Starting application on port: %d", *appPort)

	// 3. Configure Redis Options
	opts := &redis.Options{
		Addr: net.JoinHostPort(*redisHost, fmt.Sprintf("%d", *redisPort)),
	}
	if *username != "" {
		opts.Username = *username
	}
	if *password != "" {
		opts.Password = *password
	}
	// 4. Conditionally add TLS configuration
	if *useTLS {
		opts.TLSConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: *skipVerify, // Added control toggle for self-signed remote instances
		}
		log.Printf("Enabling TLS for Redis connection to %s", opts.Addr)
	}

	// 5. Connect and execute
	rdb := redis.NewClient(opts)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("error reading body: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		var data RequestBody
		if err := json.Unmarshal(b, &data); err != nil {
			log.Printf("error unmarshaling JSON: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		written := 0
		for _, item := range data.Payload {
			itemJSON, err := json.Marshal(item)
			if err != nil {
				log.Printf("error marshaling item %s: %v", item.After.ID, err)
				continue
			}

			key := fmt.Sprintf("vehicle:%s", item.After.ID)

			// Context (ctx) now properly referenced globally
			if err := rdb.JSONSet(ctx, key, "$", itemJSON).Err(); err != nil {
				log.Printf("error writing key %s to database: %v", key, err)
				continue
			}
			written++
		}

		log.Printf("Processed and wrote %d/%d items safely.", written, len(data.Payload))
		w.WriteHeader(http.StatusOK)
	})

	// Fixed variables: dereferenced flag pointer (*appPort)
	log.Printf("Starting HTTPS listener server on port %d...", *appPort)
	log.Fatal(http.ListenAndServeTLS(fmt.Sprintf(":%d", *appPort), CertPath, KeyPath, nil))
}
