package app

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/endpoint"
	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Config defines the application configuration.
type Config struct {
	rest.Config `config:",squash"`

	Services struct {
		DataURL        string `config:"data_url"`
		RestrictionURL string `config:"restriction_url"`
		EligibilityURL string `config:"eligibility_url"`
	} `config:"services"`
}

// Init initializes the REST API with all endpoints and services.
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	// Create OTel-instrumented HTTP client for service calls
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	// Initialize services
	dataSvc := service.NewDataClient(cfg.Services.DataURL, httpClient)
	restrictionSvc := service.NewRestrictionClient(cfg.Services.RestrictionURL, httpClient)
	eligibilitySvc := service.NewEligibilityClient(cfg.Services.EligibilityURL, httpClient)

	// Create API with endpoints
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.ListOrders(dataSvc),
		endpoint.PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc),
	)

	return api, nil
}
