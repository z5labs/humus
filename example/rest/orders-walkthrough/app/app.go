package app

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/example/rest/orders-walkthrough/endpoint"
	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// BuildApi creates the REST API with all endpoints registered.
// Service URLs are read from config readers.
func BuildApi(
	ctx context.Context,
	dataURL config.Reader[string],
	restrictionURL config.Reader[string],
	eligibilityURL config.Reader[string],
) (*rest.Api, error) {
	// Create OTel-instrumented HTTP client for service calls
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	// Read service URLs from config
	dataURLValue := config.Must(ctx, dataURL)
	restrictionURLValue := config.Must(ctx, restrictionURL)
	eligibilityURLValue := config.Must(ctx, eligibilityURL)

	// Initialize services
	dataSvc := service.NewDataClient(dataURLValue, httpClient)
	restrictionSvc := service.NewRestrictionClient(restrictionURLValue, httpClient)
	eligibilitySvc := service.NewEligibilityClient(eligibilityURLValue, httpClient)

	// Create API with endpoints
	api := rest.NewApi(
		"Orders API",
		"v0.0.0",
		endpoint.ListOrders(dataSvc),
		endpoint.PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc),
	)

	return api, nil
}
