// Copyright (C) 2022 Specter Ops, Inc.
//
// This file is part of AzureHound.
//
// AzureHound is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// AzureHound is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"time"

	"github.com/bloodhoundad/azurehound/v2/client/rest"
	"github.com/bloodhoundad/azurehound/v2/config"
	"github.com/bloodhoundad/azurehound/v2/constants"
	"github.com/bloodhoundad/azurehound/v2/models"
	"github.com/bloodhoundad/azurehound/v2/pipeline"
	"github.com/spf13/cobra"
)

const (
	BHEAuthSignature string = "bhesignature"
)

var (
	ErrExceededRetryLimit = errors.New("exceeded max retry limit for ingest batch, proceeding with next batch...")
)

func init() {
	configs := append(config.AzureConfig, config.BloodHoundEnterpriseConfig...)
	config.Init(startCmd, configs)
	rootCmd.AddCommand(startCmd)
}

var startCmd = &cobra.Command{
	Use:               "start",
	Short:             "Start Azure data collection service for BloodHound Enterprise",
	Run:               startCmdImpl,
	PersistentPreRunE: persistentPreRunE,
	SilenceUsage:      true,
}

func startCmdImpl(cmd *cobra.Command, args []string) {
	start(cmd.Context())
}

func start(ctx context.Context) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
	sigChan := make(chan os.Signal)
	go func() {
		stacktrace := make([]byte, 8192)
		for range sigChan {
			length := runtime.Stack(stacktrace, true)
			fmt.Println(string(stacktrace[:length]))
		}
	}()
	defer gracefulShutdown(stop)

	log.V(1).Info("testing connections")
	if azClient := connectAndCreateClient(); azClient == nil {
		exit(fmt.Errorf("azClient is unexpectedly nil"))
	} else if bheInstance, err := url.Parse(config.BHEUrl.Value().(string)); err != nil {
		exit(fmt.Errorf("unable to parse BHE url: %w", err))
	} else if bheClient, err := newSigningHttpClient(BHEAuthSignature, config.BHETokenId.Value().(string), config.BHEToken.Value().(string), config.Proxy.Value().(string)); err != nil {
		exit(fmt.Errorf("failed to create new signing HTTP client: %w", err))
	} else if err := updateClient(ctx, *bheInstance, bheClient); err != nil {
		exit(fmt.Errorf("failed to update client: %w", err))
	} else {
		log.Info("connected successfully! waiting for tasks...")
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		var (
			currentTask *models.ClientTask
		)

		for {
			select {
			case <-ticker.C:
				if currentTask != nil {
					log.V(1).Info("collection in progress...", "jobId", currentTask.Id)
					if err := checkin(ctx, *bheInstance, bheClient); err != nil {
						log.Error(err, "bloodhound enterprise service checkin failed")
					}
				} else {
					go func() {
						log.V(2).Info("checking for available collection tasks")
						if availableTasks, err := getAvailableTasks(ctx, *bheInstance, bheClient); err != nil {
							log.Error(err, "unable to fetch available tasks for azurehound")
						} else {

							// Get only the tasks that have reached their execution time
							executableTasks := []models.ClientTask{}
							now := time.Now()
							for _, task := range availableTasks {
								if task.ExectionTime.Before(now) || task.ExectionTime.Equal(now) {
									executableTasks = append(executableTasks, task)
								}
							}

							// Sort tasks in ascending order by execution time
							sort.Slice(executableTasks, func(i, j int) bool {
								return executableTasks[i].ExectionTime.Before(executableTasks[j].ExectionTime)
							})

							if len(executableTasks) == 0 {
								log.V(2).Info("there are no tasks for azurehound to complete at this time")
							} else {

								// Notify BHE instance of task start
								currentTask = &executableTasks[0]
								if err := startTask(ctx, *bheInstance, bheClient, currentTask.Id); err != nil {
									log.Error(err, "failed to start task, will retry on next heartbeat")
									currentTask = nil
									return
								}

								start := time.Now()

								// Batch data out for ingestion
								stream := listAll(ctx, azClient)
								batches := pipeline.Batch(ctx.Done(), stream, 256, 10*time.Second)
								hasIngestErr := ingest(ctx, *bheInstance, bheClient, batches)

								// Notify BHE instance of task end
								duration := time.Since(start)

								message := "Collection completed successfully"
								if hasIngestErr {
									message = "Collection completed with errors during ingest"

								}
								if err := endTask(ctx, *bheInstance, bheClient, models.JobStatusComplete, message); err != nil {
									log.Error(err, "failed to end task")
								} else {
									log.Info(message, "id", currentTask.Id, "duration", duration.String())
								}

								currentTask = nil
							}
						}
					}()
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

func ingest(ctx context.Context, bheUrl url.URL, bheClient *http.Client, in <-chan []interface{}) bool {
	endpoint := bheUrl.ResolveReference(&url.URL{Path: "/api/v2/ingest"})

	var (
		hasErrors           = false
		maxRetries          = 3
		unrecoverableErrMsg = fmt.Sprintf("ending current ingest job due to unrecoverable error while requesting %v", endpoint)
	)

	for data := range pipeline.OrDone(ctx.Done(), in) {
		body := models.IngestRequest{
			Meta: models.Meta{
				Type: "azure",
			},
			Data: data,
		}

		headers := make(map[string]string)
		headers["Prefer"] = "wait=60"

		if req, err := rest.NewRequest(ctx, "POST", endpoint, body, nil, headers); err != nil {
			log.Error(err, unrecoverableErrMsg)
			return true
		} else {
			for retry := 0; retry < maxRetries; retry++ {
				//No retries on regular err cases, only on HTTP 504 Gateway Timeout and HTTP 503 Service Unavailable
				if response, err := bheClient.Do(req); err != nil {
					log.Error(err, unrecoverableErrMsg)
					return true
				} else if response.StatusCode == http.StatusGatewayTimeout || response.StatusCode == http.StatusServiceUnavailable {
					backoff := math.Pow(5, float64(retry+1))
					time.Sleep(time.Second * time.Duration(backoff))
					if retry == maxRetries-1 {
						log.Error(ErrExceededRetryLimit, "")
						hasErrors = true
					}
					continue
				} else if response.StatusCode != http.StatusAccepted {
					if bodyBytes, err := io.ReadAll(response.Body); err != nil {
						log.Error(fmt.Errorf("received unexpected response code from %v: %s; failure reading response body", endpoint, response.Status), unrecoverableErrMsg)
					} else {
						log.Error(fmt.Errorf("received unexpected response code from %v: %s %s", req.URL, response.Status, bodyBytes), unrecoverableErrMsg)
					}
					return true
				}
			}
		}
	}
	return hasErrors
}

// TODO: create/use a proper bloodhound client
func do(bheClient *http.Client, req *http.Request) (*http.Response, error) {
	if res, err := bheClient.Do(req); err != nil {
		return nil, fmt.Errorf("failed to request %v: %w", req.URL, err)
	} else if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusBadRequest {
		var body json.RawMessage
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("received unexpected response code from %v: %s; failure reading response body", req.URL, res.Status)
		} else {
			return nil, fmt.Errorf("received unexpected response code from %v: %s %s", req.URL, res.Status, body)
		}
	} else {
		return res, nil
	}
}

func getAvailableTasks(ctx context.Context, bheUrl url.URL, bheClient *http.Client) ([]models.ClientTask, error) {
	var (
		endpoint = bheUrl.ResolveReference(&url.URL{Path: "/api/v1/clients/availabletasks"})
		response []models.ClientTask
	)

	if req, err := rest.NewRequest(ctx, "GET", endpoint, nil, nil, nil); err != nil {
		return nil, err
	} else if res, err := do(bheClient, req); err != nil {
		return nil, err
	} else if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	} else {
		return response, nil
	}
}

func checkin(ctx context.Context, bheUrl url.URL, bheClient *http.Client) error {
	endpoint := bheUrl.ResolveReference(&url.URL{Path: "/api/v2/jobs/current"})

	if req, err := rest.NewRequest(ctx, "GET", endpoint, nil, nil, nil); err != nil {
		return err
	} else if _, err := do(bheClient, req); err != nil {
		return err
	} else {
		return nil
	}
}

func startTask(ctx context.Context, bheUrl url.URL, bheClient *http.Client, taskId int) error {
	log.Info("beginning collection task", "id", taskId)
	var (
		endpoint = bheUrl.ResolveReference(&url.URL{Path: "/api/v1/clients/starttask"})
		body     = map[string]int{
			"id": taskId,
		}
	)

	if req, err := rest.NewRequest(ctx, "POST", endpoint, body, nil, nil); err != nil {
		return err
	} else if _, err := do(bheClient, req); err != nil {
		return err
	} else {
		return nil
	}
}

func endTask(ctx context.Context, bheUrl url.URL, bheClient *http.Client, status models.JobStatus, message string) error {
	endpoint := bheUrl.ResolveReference(&url.URL{Path: "/api/v2/jobs/end"})

	body := models.CompleteJobRequest{
		Status:  status.String(),
		Message: message,
	}

	if req, err := rest.NewRequest(ctx, "POST", endpoint, body, nil, nil); err != nil {
		return err
	} else if _, err := do(bheClient, req); err != nil {
		return err
	} else {
		return nil
	}
}

func updateClient(ctx context.Context, bheUrl url.URL, bheClient *http.Client) error {
	endpoint := bheUrl.ResolveReference(&url.URL{Path: "/api/v1/clients/update"})
	if addr, err := dial(bheUrl.String()); err != nil {
		return err
	} else {
		// hostname is nice to have but we don't really need it
		hostname, _ := os.Hostname()

		body := models.UpdateClientRequest{
			Address:  addr,
			Hostname: hostname,
			Version:  constants.Version,
		}

		log.V(2).Info("updating client info", "info", body)

		if req, err := rest.NewRequest(ctx, "PUT", endpoint, body, nil, nil); err != nil {
			return err
		} else if _, err := do(bheClient, req); err != nil {
			return err
		} else {
			return nil
		}
	}
}
