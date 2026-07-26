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
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Declare a global context background for Redis commands
var ctx = context.Background()

// PayloadItem mirrors a CockroachDB changefeed (CDC) envelope. "After" is
// kept as raw JSON because its shape varies by source table (users,
// vehicles, rides, promo_codes, user_promo_codes,
// vehicle_location_histories); "Topic" tells us which table it came from.
type PayloadItem struct {
	After   json.RawMessage `json:"after"`
	Key     []interface{}   `json:"key"`
	Topic   string          `json:"topic"`
	Updated string          `json:"updated"`
}

type RequestBody struct {
	Payload []PayloadItem `json:"payload"`
	Length  int           `json:"length"`
}

const (
	CertPath string = "cert.pem"
	KeyPath  string = "key.pem"

	// maxBatchSize caps how many JSONSet calls are flushed to Redis in a
	// single pipeline Exec. Request payloads containing more items than this
	// are written across multiple pipelined chunks instead of one unbounded
	// pipeline.
	maxBatchSize = 200
)

// Key prefixes for each entity, matching the source table names.
const (
	prefixPromoCode           = "takearide:promo_code"
	prefixRide                = "takearide:ride"
	prefixUser                = "takearide:user"
	prefixVehicleLocationHist = "takearide:vehicle_location_history"
	prefixVehicle             = "takearide:vehicle"
	prefixUserPromoCode       = "takearide:user_promo_code"
)

// topicToPrefix maps a changefeed topic (which CockroachDB names after the
// source table) to the corresponding Redis key prefix.
var topicToPrefix = map[string]string{
	"promo_codes":                prefixPromoCode,
	"rides":                      prefixRide,
	"users":                      prefixUser,
	"vehicle_location_histories": prefixVehicleLocationHist,
	"vehicles":                   prefixVehicle,
	"user_promo_codes":           prefixUserPromoCode,
}

// detectPrefix maps a payload item to a key prefix using its "topic" field.
// Changefeed topics are sometimes namespaced (e.g. "database.schema.table"
// or with a sink-specific suffix), so we normalize by taking the last
// dot-separated segment and lowercasing before looking it up.
func detectPrefix(topic string) (string, error) {
	name := strings.ToLower(topic)
	if i := strings.LastIndex(name, "."); i != -1 {
		name = name[i+1:]
	}

	prefix, ok := topicToPrefix[name]
	if !ok {
		return "", fmt.Errorf("unrecognized topic %q", topic)
	}
	return prefix, nil
}

// buildID reconstructs the entity's natural primary key as a single string,
// joining composite key columns with "_" so the result is safe to use as a
// Redis key segment.
func buildID(prefix string, after map[string]interface{}) (string, error) {
	str := func(k string) string {
		v, ok := after[k]
		if !ok || v == nil {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}

	join := func(parts ...string) (string, error) {
		for _, p := range parts {
			if p == "" {
				return "", fmt.Errorf("missing key component in %v", after)
			}
		}
		return strings.Join(parts, "_"), nil
	}

	switch prefix {
	case prefixUser:
		// pkey (city, id)
		return join(str("city"), str("id"))
	case prefixVehicle:
		// pkey (city, id)
		return join(str("city"), str("id"))
	case prefixRide:
		// pkey (city, id)
		return join(str("city"), str("id"))
	case prefixPromoCode:
		// pkey (code)
		return join(str("code"))
	case prefixUserPromoCode:
		// pkey (city, user_id, code)
		return join(str("city"), str("user_id"), str("code"))
	case prefixVehicleLocationHist:
		// pkey (city, ride_id, timestamp)
		return join(str("city"), str("ride_id"), str("timestamp"))
	default:
		return "", fmt.Errorf("unknown prefix %q", prefix)
	}
}

// timestampFieldsByPrefix lists the "after" fields, per entity, that hold
// ISO-ish timestamp strings and should be rewritten as epoch millis so
// RediSearch can index them as NUMERIC (range/sort queries) instead of TEXT.
var timestampFieldsByPrefix = map[string][]string{
	prefixVehicle:             {"creation_time"},
	prefixRide:                {"start_time", "end_time"},
	prefixVehicleLocationHist: {"timestamp"},
	prefixPromoCode:           {"creation_time", "expiration_time"},
	prefixUserPromoCode:       {"timestamp"},
}

// timeLayouts are tried in order against timestamp strings coming out of
// CockroachDB, which are typically RFC3339-ish but without a timezone offset
// and with a variable-length fractional-second component.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format %q", s)
}

// convertTimestamps rewrites every timestamp field defined for this prefix,
// in place, from an ISO string to epoch milliseconds (int64). Fields that
// are absent, null, or already non-string (e.g. re-processed once already)
// are left untouched rather than treated as errors.
func convertTimestamps(prefix string, after map[string]interface{}) {
	for _, field := range timestampFieldsByPrefix[prefix] {
		v, ok := after[field]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		t, err := parseTimestamp(s)
		if err != nil {
			log.Printf("could not convert %s.%s=%q to epoch millis: %v", prefix, field, s, err)
			continue
		}
		after[field] = t.UnixMilli()
	}
}

// addGeoAttribute builds a "geo" field formatted as "lon,lat", which is the
// string format RediSearch's GEO field type expects for FT.SEARCH GEORADIUS
// / geo-filter queries. Only vehicle_location_history documents carry lat
// and long columns.
func addGeoAttribute(prefix string, after map[string]interface{}) {
	if prefix != prefixVehicleLocationHist {
		return
	}

	toFloat := func(v interface{}) (float64, bool) {
		switch t := v.(type) {
		case float64:
			return t, true
		case string:
			f, err := strconv.ParseFloat(t, 64)
			return f, err == nil
		default:
			return 0, false
		}
	}

	lat, latOK := toFloat(after["lat"])
	lon, lonOK := toFloat(after["long"])
	if !latOK || !lonOK {
		return
	}

	after["geo"] = fmt.Sprintf("%g,%g", lon, lat)
}

func convertCreditCard(prefix string, after map[string]interface{}) {
	if prefix != prefixUser {
		return
	}
	if v, ok := after["credit_card"]; ok && v != nil {
		after["credit_card"] = toInt64(v)
	}
}

func toInt64(v interface{}) int64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		i, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

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
	// Safely check bool values
	if *useTLS {
		host, _, err := net.SplitHostPort(opts.Addr)
		if err != nil {
			host = opts.Addr // Fallback if opts.Addr is just a hostname without a port
		}

		opts.TLSConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         host,
			InsecureSkipVerify: *skipVerify,
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

		// First pass: parse/validate/transform every item and produce the
		// final (key, JSON) pair to write. No Redis calls happen here.
		type pendingWrite struct {
			key      string
			itemJSON []byte
		}
		toWrite := make([]pendingWrite, 0, len(data.Payload))

		for _, item := range data.Payload {
			prefix, err := detectPrefix(item.Topic)
			if err != nil {
				log.Printf("skipping item: %v", err)
				continue
			}

			var after map[string]interface{}
			if err := json.Unmarshal(item.After, &after); err != nil {
				log.Printf("error unmarshaling 'after' document for topic %q: %v", item.Topic, err)
				continue
			}

			id, err := buildID(prefix, after)
			if err != nil {
				log.Printf("skipping %s item, could not build key: %v", prefix, err)
				continue
			}

			// Mutate the "after" document for storage only after the key has
			// been derived, so index-friendly rewrites never change key naming.
			convertTimestamps(prefix, after)
			addGeoAttribute(prefix, after)
			convertCreditCard(prefix, after)

			afterJSON, err := json.Marshal(after)
			if err != nil {
				log.Printf("error re-marshaling 'after' document (prefix %s, id %s): %v", prefix, id, err)
				continue
			}
			key := fmt.Sprintf("%s:%s", prefix, id)
			toWrite = append(toWrite, pendingWrite{key: key, itemJSON: afterJSON})
		}

		// Second pass: flush the writes in pipelined chunks of at most
		// maxBatchSize. Chunking keeps any single pipeline (and the request
		// payloads that produce very large batches) bounded, so one huge CDC
		// batch can't build an oversized pipeline or dominate Redis latency.
		written := 0
		for start := 0; start < len(toWrite); start += maxBatchSize {
			end := start + maxBatchSize
			if end > len(toWrite) {
				end = len(toWrite)
			}
			chunk := toWrite[start:end]

			pipe := rdb.Pipeline()
			for _, w := range chunk {
				pipe.JSONSet(ctx, w.key, "$", w.itemJSON)
			}

			cmders, err := pipe.Exec(ctx)
			if err != nil && err != redis.Nil {
				log.Printf("pipeline exec returned an error for batch [%d:%d] (some writes may still have succeeded): %v", start, end, err)
			}

			for i, cmder := range cmders {
				if err := cmder.Err(); err != nil {
					log.Printf("error writing key %s to database: %v", chunk[i].key, err)
					continue
				}
				written++
			}
		}

		//log.Printf("Processed and wrote %d/%d items safely.", written, len(data.Payload))
		w.WriteHeader(http.StatusOK)
	})

	// Fixed variables: dereferenced flag pointer (*appPort)
	log.Printf("Starting HTTPS listener server on port %d...", *appPort)
	log.Fatal(http.ListenAndServeTLS(fmt.Sprintf(":%d", *appPort), CertPath, KeyPath, nil))
}
