package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"github.com/redis/go-redis/v9"
)

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
	port := 3000
	if len(os.Args) >= 2 {
		var err error
		port, err = strconv.Atoi(os.Args[1])
		if err != nil {
			panic(err)
		}
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
        log.Printf("rdb: %v",rdb)
        ctx := context.Background()
        rdb.Set(ctx,"otkey","so much for testing",0)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
                log.Printf("%s", b)

		var data RequestBody
		if err := json.Unmarshal(b, &data); err != nil {
			log.Printf("error unmarshaling JSON: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		written := 0
                log.Printf("\n\ndata.Payload = %v",data.Payload)
		for _, item := range data.Payload {
			itemJSON, err := json.Marshal(item)
			if err != nil {
				log.Printf("error marshaling item %s: %v", item.After.ID, err)
				continue
			}

			key := fmt.Sprintf("vehicle:%s", item.After.ID)
			if err := rdb.JSONSet(ctx, key,"$", itemJSON).Err(); err != nil {
				log.Printf("error writing key %s to Redis: %v", key, err)
				continue
			}
			written++
		}

		log.Printf("wrote %d/%d items to Redis", written, len(data.Payload))
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("starting server on port %d", port)
	log.Fatal(http.ListenAndServeTLS(fmt.Sprintf(":%d", port), CertPath, KeyPath, nil))
}
