package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/golang-jwt/jwt/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	listenAddress = flag.String("listen", "127.0.0.1:8550", "Address to listen on")
	enginesString = flag.String("engines", "http://127.0.0.1:8551,http://127.0.0.1:8552,http://127.0.0.1:8553", "Engine addresses")
	threshold     = flag.Int("threshold", 2, "Threshold")
	secret        = flag.String("secret", "fe5ca2805dca7dc85dd95c0b78f52648578c605f9e108fdda43a747dfa09b5e1", "JWT secret")

	httpClient  = &http.Client{Timeout: 5 * time.Second}
	engines     []Engine
	primary     int
	primaryLock sync.Mutex

	summary = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Name: "response_time",
		Help: "A histogram of execution node response times.",
	}, []string{"node", "api"})
)

func main() {
	flag.Parse()

	var engineStrings = strings.Split(*enginesString, ",")

	for i := range engineStrings {
		engines = append(engines, Engine{
			Url: engineStrings[i],
		})
	}

	selectPrimary()

	go func() {
		for {
			time.Sleep(5 * time.Second)
			selectPrimary()
		}
	}()

	http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()

		primaryLock.Lock()
		var primary = primary
		primaryLock.Unlock()

		var engineRequest EngineRequest
		_ = json.Unmarshal(body, &engineRequest)

		if strings.HasPrefix(engineRequest.Method, "engine_") {
			switch engineRequest.Method {
			case "engine_newPayloadV1", "engine_forkchoiceUpdatedV1":
				// Forward to all, but only wait for threshold
				var resultChannel = make(chan Result)

				for i := range engines {
					go func(i int) {
						var engineResponse EngineResponse

						var engineResponseRaw, err, duration = forward(engines[i].Url, request.Header.Clone(), body)
						if err == nil {
							_ = json.Unmarshal(engineResponseRaw, &engineResponse)
						}

						var status string

						switch engineRequest.Method {
						case "engine_newPayloadV1":
							var forkchoiceStateV1 ForkchoiceStateV1
							_ = json.Unmarshal(engineResponse.Result, &forkchoiceStateV1)
							status = forkchoiceStateV1.Status
						case "engine_forkchoiceUpdatedV1":
							var payloadStatusV1 PayloadStatusV1
							_ = json.Unmarshal(engineResponse.Result, &payloadStatusV1)
							status = payloadStatusV1.PayloadStatus.Status
						}

						resultChannel <- Result{Engine: engines[i].Url, Status: status, Duration: duration, Response: engineResponseRaw}
						summary.With(prometheus.Labels{"node": engines[i].Url, "api": engineRequest.Method}).Observe(float64(duration.Milliseconds()))
					}(i)
				}

				var doneChannel = make(chan struct{})

				go func() {
					var valid []Result
					var nonValid []Result
					var logString string
					var once sync.Once

					for {
						var result = <-resultChannel

						if result.Status == "VALID" {
							valid = append(valid, result)
						} else {
							nonValid = append(nonValid, result)
						}

						if len(valid) >= *threshold {
							once.Do(func() {
								_, _ = response.Write(valid[0].Response)
								close(doneChannel)
								logString = fmt.Sprintf("VALID %s", engineRequest.Method)
							})
						}

						if len(valid)+len(nonValid) == len(engines) {
							once.Do(func() {
								switch engineRequest.Method {
								case "engine_newPayloadV1":
									_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"SYNCING","latestValidHash":null,"validationError":null}}`))
								case "engine_forkchoiceUpdatedV1":
									_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"payloadStatus":{"status":"SYNCING","latestValidHash":null,"validationError":null},"payloadId":null}}`))
								}

								close(doneChannel)
								logString = fmt.Sprintf("SYNCING %s", engineRequest.Method)
							})

							fmt.Println(logString + sprintfResults(append(valid, nonValid...)))
							break
						}
					}
				}()

				<-doneChannel
			default:
				// Forward to all, but only wait for primary
				var primaryResponseChannel = make(chan []byte)

				for i := range engines {
					go func(i int) {
						var response, _, _ = forward(engines[i].Url, request.Header.Clone(), body)

						if i == primary {
							primaryResponseChannel <- response
						}
					}(i)
				}

				_, _ = response.Write(<-primaryResponseChannel)
			}
		} else {
			// Forward to primary
			var engineResponse, _, _ = forward(engines[primary].Url, request.Header.Clone(), body)
			_, _ = response.Write(engineResponse)
		}
	})

	go func() {
		err := http.ListenAndServe(*listenAddress, nil)
		if err != nil {
			fmt.Printf("Failed to listen on %s\n", *listenAddress)
			os.Exit(1)
		}
	}()

	fmt.Printf("Started Heliopolis, listening on %s\n", *listenAddress)

	// Handle interrupt gracefully
	var interrupt = make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	<-interrupt
}

func forward(address string, header http.Header, body []byte) ([]byte, error, time.Duration) {
	request, _ := http.NewRequest("POST", address, bytes.NewBuffer(body))
	request.Header = header
	request.Header.Set("Accept-Encoding", "identity")

	var start = time.Now()
	response, err := httpClient.Do(request)
	var duration = time.Since(start).Round(time.Millisecond)
	if err != nil {
		return []byte{}, err, 0
	}

	responseRaw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return responseRaw, nil, duration
}

// Return engine with the highest block
func selectPrimary() {
	engineCheck()

	for i := range engines {
		if engines[i].BlockHeight == -1 {
			fmt.Printf("Engine %s seems to be offline!\n", engines[i].Url)
			continue
		}

		if engines[i].BlockHeight-engines[primary].BlockHeight >= 3 {
			primaryLock.Lock()
			primary = i
			primaryLock.Unlock()
			fmt.Printf("Selected %s as new primary\n", engines[primary].Url)
		}
	}
}

func engineCheck() {
	var token = jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iat": time.Now().Unix(),
	})

	var decodeString, _ = hex.DecodeString(*secret)
	var tokenString, _ = token.SignedString(decodeString)

	var waitGroup sync.WaitGroup
	waitGroup.Add(len(engines))

	for i := range engines {
		go func(i int) {
			request, _ := http.NewRequest("POST", engines[i].Url, bytes.NewBuffer([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)))
			request.Header.Add("Content-Type", "application/json")
			request.Header.Add("Authorization", "Bearer "+tokenString)

			response, err := httpClient.Do(request)
			if err != nil {
				engines[i].BlockHeight = -1
			} else {
				var ethBlockNumberResponse BlockNumberResponse
				responseRaw, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				_ = json.Unmarshal(responseRaw, &ethBlockNumberResponse)

				var blockNumber, _ = strconv.ParseInt(ethBlockNumberResponse.Result, 0, 64)
				engines[i].BlockHeight = blockNumber
			}

			waitGroup.Done()
		}(i)
	}

	waitGroup.Wait()
}

func sprintfResults(results []Result) string {
	var logString string

	for _, result := range results {
		logString += fmt.Sprintf(" (%s %s %s)", result.Engine, result.Status, result.Duration)
	}

	return logString
}
