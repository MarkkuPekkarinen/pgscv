// Package pgscv is a pgSCV main helper
package pgscv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	net_http "net/http"
	"strings"
	"sync"
	"time"

	"github.com/cherts/pgscv/discovery"
	sd "github.com/cherts/pgscv/internal/discovery/service"
	"github.com/cherts/pgscv/internal/http"
	"github.com/cherts/pgscv/internal/log"
	"github.com/cherts/pgscv/internal/model"
	"github.com/cherts/pgscv/internal/service"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const pgSCVSubscriber = "pgscv_subscriber"

type target struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Start is the application's starting point.
func Start(ctx context.Context, config *Config) error {
	log.Debug("start application")

	serviceRepo := service.NewRepository()

	serviceConfig := service.Config{
		NoTrackMode:        config.NoTrackMode,
		ConnDefaults:       config.Defaults,
		ConnsSettings:      config.ServicesConnsSettings,
		DatabasesRE:        config.DatabasesRE,
		DisabledCollectors: config.DisableCollectors,
		CollectorsSettings: config.CollectorsSettings,
		CollectTopTable:    config.CollectTopTable,
		CollectTopIndex:    config.CollectTopIndex,
		CollectTopQuery:    config.CollectTopQuery,
		SkipConnErrorMode:  config.SkipConnErrorMode,
		ConnTimeout:        config.ConnTimeout,
		ThrottlingInterval: config.ThrottlingInterval,
		ConcurrencyLimit:   config.ConcurrencyLimit,
	}

	if len(config.ServicesConnsSettings) == 0 && config.DiscoveryServices == nil {
		return errors.New("no services defined")
	}

	// fulfill service repo using passed services
	serviceRepo.AddServicesFromConfig(serviceConfig)

	// setup exporters for all services
	err := serviceRepo.SetupServices(serviceConfig)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	errCh := make(chan error, 2)
	defer close(errCh)
	if config.DiscoveryServices != nil {
		for _, ds := range *config.DiscoveryServices {
			wg.Add(1)
			go func() {
				err := ds.Start(ctx, errCh)
				if err != nil {
					errCh <- err
				}
				wg.Done()
			}()
			switch dt := ds.(type) {
			case *sd.YandexDiscovery:
				err := subscribeYandex(&ds, config, serviceRepo)
				if err != nil {
					cancel()
					return err
				}
			default:
				log.Infof("unknown discovery type %T", dt)
			}

		}
	}

	// Start HTTP metrics listener.
	wg.Add(1)
	go func() {
		if err := runMetricsListener(ctx, config, serviceRepo); err != nil {
			errCh <- err
		}
		wg.Done()
	}()

	// Waiting for errors or context cancelling.
	for {
		select {
		case <-ctx.Done():
			log.Info("exit signaled, stop application")
			cancel()
			wg.Wait()
			return nil
		case e := <-errCh:
			cancel()
			wg.Wait()
			return e
		}
	}
}

func subscribeYandex(ds *discovery.Discovery, config *Config, serviceRepo *service.Repository) error {
	err := (*ds).Subscribe(pgSCVSubscriber,
		// addService
		func(services map[string]discovery.Service) error {
			constLabels := make(map[string]*map[string]string)
			targetLabels := make(map[string]*map[string]string)
			serviceDiscoveryConfig := service.Config{
				NoTrackMode:        config.NoTrackMode,
				ConnDefaults:       config.Defaults,
				DisabledCollectors: config.DisableCollectors,
				CollectorsSettings: config.CollectorsSettings,
				CollectTopTable:    config.CollectTopTable,
				CollectTopIndex:    config.CollectTopIndex,
				CollectTopQuery:    config.CollectTopQuery,
				SkipConnErrorMode:  config.SkipConnErrorMode,
				ConstLabels:        &constLabels,
				TargetLabels:       &targetLabels,
				ConnTimeout:        config.ConnTimeout,
				ConcurrencyLimit:   config.ConcurrencyLimit,
			}
			var cs = make(service.ConnsSettings, len(services))
			for serviceID, svc := range services {
				cs[serviceID] = service.ConnSetting{
					ServiceType: model.ServiceTypePostgresql,
					Conninfo:    svc.DSN,
				}
				constLabels[serviceID] = &svc.ConstLabels
				targetLabels[serviceID] = &svc.TargetLabels
			}
			serviceDiscoveryConfig.ConnsSettings = cs
			serviceRepo.AddServicesFromConfig(serviceDiscoveryConfig)
			err := serviceRepo.SetupServices(serviceDiscoveryConfig)
			if err != nil {
				return err
			}
			return nil
		},
		// removeService
		func(serviceIds []string) error {
			for _, serviceID := range serviceIds {
				log.Infof("unregister service [%s]", serviceID)
				serviceRepo.RemoveService(serviceID)
			}
			return nil
		},
	)
	return err
}

// getMetricsHandler return http handler function to /metrics endpoint
func getMetricsHandler(repository *service.Repository, throttlingInterval *int) func(w net_http.ResponseWriter, r *net_http.Request) {
	throttle := struct {
		sync.RWMutex
		lastScrapeTime map[string]time.Time
	}{
		lastScrapeTime: make(map[string]time.Time),
	}

	return func(w net_http.ResponseWriter, r *net_http.Request) {
		target := r.URL.Query().Get("target")
		if throttlingInterval != nil && *throttlingInterval > 0 {
			throttle.RLock()
			t, ok := throttle.lastScrapeTime[target]
			throttle.RUnlock()
			if ok {
				if time.Now().Sub(t) < time.Duration(*throttlingInterval)*time.Second {
					w.WriteHeader(http.StatusOK)
					log.Warnf("Skip scraping, method: %s, proto: %s, request_uri: %s, user_agent: %s, remote_addr: %s", r.Method, r.Proto, r.RequestURI, r.UserAgent(), r.RemoteAddr)
					return
				}
			}
			throttle.Lock()
			throttle.lastScrapeTime[target] = time.Now()
			throttle.Unlock()
		}
		if target == "" {
			h := promhttp.InstrumentMetricHandler(
				prometheus.DefaultRegisterer, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}),
			)
			h.ServeHTTP(w, r)
		} else {
			registry := repository.GetRegistry(target)
			if registry == nil {
				net_http.Error(w, fmt.Sprintf("target %s not registered", target), http.StatusNotFound)
				return
			}
			h := promhttp.InstrumentMetricHandler(
				registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
			)
			h.ServeHTTP(w, r)
		}
	}
}

// getTargetsHandler return http handler function to /targets endpoint
func getTargetsHandler(repository *service.Repository, urlPrefix string, enableTLS bool) func(w net_http.ResponseWriter, r *net_http.Request) {
	return func(w net_http.ResponseWriter, r *net_http.Request) {
		var url string
		if urlPrefix != "" {
			url = strings.Trim(urlPrefix, "/")
		} else {
			if enableTLS {
				url = fmt.Sprintf("https://%s", r.Host)
			} else {
				url = r.Host
			}
		}
		repository.RLock()
		defer repository.RUnlock()
		groupedTargets := make(map[string]*target)

		for _, service := range repository.Services {
			targetURL := fmt.Sprintf("%s/metrics?target=%s", url, service.ServiceID)
			if service.TargetLabels == nil {
				if _, exists := groupedTargets["no_labels"]; !exists {
					groupedTargets["no_labels"] = &target{
						Targets: []string{},
					}
				}
				groupedTargets["no_labels"].Targets = append(groupedTargets["no_labels"].Targets, targetURL)
			} else {
				labelsKey := fmt.Sprintf("%v", *service.TargetLabels)
				if _, exists := groupedTargets[labelsKey]; !exists {
					groupedTargets[labelsKey] = &target{
						Targets: []string{},
						Labels:  *service.TargetLabels,
					}
				}
				groupedTargets[labelsKey].Targets = append(groupedTargets[labelsKey].Targets, targetURL)
			}
		}

		var result []target
		for _, target := range groupedTargets {
			result = append(result, *target)
		}

		jsonData, err := json.Marshal(result)
		if err != nil {
			net_http.Error(w, err.Error(), net_http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(jsonData)
		if err != nil {
			log.Error(err.Error())
		}
	}
}

// runMetricsListener start HTTP listener accordingly to passed configuration.
func runMetricsListener(ctx context.Context, config *Config, repository *service.Repository) error {
	sCfg := http.ServerConfig{
		Addr:       config.ListenAddress,
		AuthConfig: config.AuthConfig,
	}
	srv := http.NewServer(sCfg, getMetricsHandler(repository, config.ThrottlingInterval), getTargetsHandler(repository, config.URLPrefix, config.AuthConfig.EnableTLS))

	errCh := make(chan error)
	defer close(errCh)

	// Run default listener.
	go func() {
		errCh <- srv.Serve()
	}()

	// Waiting for errors or context cancelling.
	for {
		select {
		case <-ctx.Done():
			log.Info("exit signaled, stop metrics listener")
			return nil
		case err := <-errCh:
			return err
		}
	}
}
