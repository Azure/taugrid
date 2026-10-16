// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kustoquery

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/kql"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/taugrid/core/expkusto"
)

type SDKClient struct {
	Endpoint  string
	Database  string
	queryJSON func(ctx context.Context, database, kql string) (string, error)
	newQuery  func(endpoint string) (func(context.Context, string, string) (string, error), error)
	initOnce  sync.Once
	run       func(context.Context, string, string) (string, error)
	initErr   error
}

type sharedADXQuery struct {
	once sync.Once
	run  func(context.Context, string, string) (string, error)
	err  error
}

var sharedADXQueries sync.Map

func (c *SDKClient) Query(ctx context.Context, query string) ([]Row, error) {
	raw, err := c.RawQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := ParseRows([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse kusto query output: %w", err)
	}
	return rows, nil
}

func (c *SDKClient) RawQuery(ctx context.Context, query string) (string, error) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return "", ErrNoQueryCommand
	}
	database := firstNonEmpty(c.Database, expkusto.DefaultDatabase)
	run := c.queryJSON
	if run == nil {
		c.initOnce.Do(func() {
			factory := c.newQuery
			if factory == nil {
				factory = newADXQuery
			}
			c.run, c.initErr = factory(endpoint)
		})
		if c.initErr != nil {
			return "", c.initErr
		}
		run = c.run
	}
	raw, err := run(ctx, database, query)
	if err != nil {
		return "", fmt.Errorf("execute kusto query (endpoint=%s database=%s): %w", endpoint, database, err)
	}
	return raw, nil
}

func NewRawSDKQuery(endpoint, database string) func(context.Context, string) (string, error) {
	client := &SDKClient{Endpoint: endpoint, Database: database}
	return client.RawQuery
}

func newADXQuery(endpoint string) (func(context.Context, string, string) (string, error), error) {
	value, _ := sharedADXQueries.LoadOrStore(endpoint, &sharedADXQuery{})
	shared := value.(*sharedADXQuery)
	return func(ctx context.Context, database, query string) (string, error) {
		shared.once.Do(func() {
			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				shared.err = fmt.Errorf("create Azure credential: %w", err)
				return
			}
			client, err := azkustodata.New(
				azkustodata.NewConnectionStringBuilder(endpoint).WithTokenCredential(cred),
			)
			if err != nil {
				shared.err = fmt.Errorf("create ADX client: %w", err)
				return
			}
			shared.run = func(ctx context.Context, database, query string) (string, error) {
				return client.QueryToJson(ctx, database, kql.New("").AddUnsafe(query))
			}
		})
		if shared.err != nil {
			return "", shared.err
		}
		return shared.run(ctx, database, query)
	}, nil
}
