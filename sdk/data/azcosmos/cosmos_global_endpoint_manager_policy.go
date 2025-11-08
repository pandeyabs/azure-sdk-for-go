// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package azcosmos

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	azlog "github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/internal/log"
)

type globalEndpointManagerPolicy struct {
	gem  *globalEndpointManager
	once sync.Once
}

func (p *globalEndpointManagerPolicy) Do(req *policy.Request) (*http.Response, error) {
	var err error
	p.once.Do(func() {
		log.Write(azlog.EventRequest, fmt.Sprintf("\n===== FIRST REQUEST - once.Do() Triggered =====\nURL: %s\nMethod: %s\nAbout to call gem.Update(forceRefresh=true)\n=====\n",
			req.Raw().URL.String(), req.Raw().Method))
		// Use the same context, but without the cancellation signal.
		// We DO want to preserve things like context values, but the GEM update needs to complete fully, even if the user cancels the triggering request.
		err = p.gem.Update(context.WithoutCancel(req.Raw().Context()), true)
		log.Write(azlog.EventRequest, fmt.Sprintf("\n===== FIRST REQUEST - once.Do() Completed =====\nUpdate Error: %v\n=====\n", err))
	})
	if p.gem.ShouldRefresh() {
		log.Write(azlog.EventRequest, fmt.Sprintf("\n===== ShouldRefresh = TRUE =====\nURL: %s\nStarting background gem.Update(forceRefresh=false)\n=====\n",
			req.Raw().URL.String()))
		go func() {
			// Use the same context, but without the cancellation signal.
			// We DO want to preserve things like context values, but the GEM update needs to complete fully, even if the user cancels the triggering request.
			updateErr := p.gem.Update(context.WithoutCancel(req.Raw().Context()), false)
			log.Write(azlog.EventRequest, fmt.Sprintf("\n===== Background Update Completed =====\nUpdate Error: %v\n=====\n", updateErr))
		}()
	} else {
		log.Write(azlog.EventRequest, fmt.Sprintf("\n===== ShouldRefresh = FALSE =====\nURL: %s\nSkipping background update\n=====\n",
			req.Raw().URL.String()))
	}
	if p.gem.CanUseMultipleWriteLocations() {
		req.Raw().Header.Set(cosmosHeaderAllowTentativeWrites, "true")
	}
	if err != nil {
		return nil, err
	}
	return req.Next()
}
