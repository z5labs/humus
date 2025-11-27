package endpoint

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"
)

// ListOrders creates the GET /v1/orders endpoint.
func ListOrders(dataSvc DataService) rest.ApiOption {
	handler := &listOrdersHandler{dataSvc: dataSvc}

	return rest.Operation(
		http.MethodGet,
		rest.BasePath("/v1").Segment("orders"),
		rest.ReturnJson(
			rest.ConsumeNothing(handler),
		),
		rest.QueryParam("accountNumber", rest.Required()),
		rest.QueryParam("after"),
		rest.QueryParam("limit", rest.Regex(regexp.MustCompile(`^[0-9]+$`))),
		rest.QueryParam("status", rest.Regex(regexp.MustCompile(`^(pending|processing|completed|failed)$`))),
	)
}

type listOrdersHandler struct {
	dataSvc DataService
}

func (h *listOrdersHandler) Produce(ctx context.Context) (*ListOrdersResponse, error) {
	accountNumberValues := rest.QueryParamValue(ctx, "accountNumber")
	accountNumber := ""
	if len(accountNumberValues) > 0 {
		accountNumber = accountNumberValues[0]
	}

	afterValues := rest.QueryParamValue(ctx, "after")
	afterCursor := ""
	if len(afterValues) > 0 {
		afterCursor = afterValues[0]
	}

	limitValues := rest.QueryParamValue(ctx, "limit")
	limitStr := ""
	if len(limitValues) > 0 {
		limitStr = limitValues[0]
	}

	statusValues := rest.QueryParamValue(ctx, "status")
	statusStr := ""
	if len(statusValues) > 0 {
		statusStr = statusValues[0]
	}

	// Default limit
	limit := 10
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	// Decode cursor if provided
	cursor := ""
	if afterCursor != "" {
		decoded, err := base64.StdEncoding.DecodeString(afterCursor)
		if err == nil {
			cursor = string(decoded)
		}
	}

	// Parse status filter (empty string means no filter)
	status := service.OrderStatus(statusStr)

	// Query data service
	result, err := h.dataSvc.Query(ctx, &service.QueryRequest{
		AccountID: accountNumber,
		Status:    status,
		Cursor:    cursor,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}

	// Convert service orders to endpoint orders
	orders := make([]Order, len(result.Orders))
	for i, svcOrder := range result.Orders {
		orders[i] = orderFromService(svcOrder)
	}

	// Build response with cursor-based pagination
	response := &ListOrdersResponse{
		Orders: orders,
		PageInfo: PageInfo{
			HasNextPage: result.HasMore,
		},
	}

	// Encode next cursor if there are more results
	if result.HasMore && result.NextCursor != "" {
		response.PageInfo.EndCursor = base64.StdEncoding.EncodeToString([]byte(result.NextCursor))
	}

	return response, nil
}
