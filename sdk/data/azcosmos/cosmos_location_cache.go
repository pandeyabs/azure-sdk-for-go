// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package azcosmos

import (
	"fmt"
	"net/url"
	"sync"
	"time"

	azlog "github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/internal/log"
)

const defaultExpirationTime time.Duration = time.Minute * 5

const (
	none requestedOperations = iota
	read
	write
	all
)

type requestedOperations int

type locationUnavailabilityInfo struct {
	lastCheckTime  time.Time
	unavailableOps requestedOperations
}

type databaseAccountLocationsInfo struct {
	prefLocations                 []string
	availWriteLocations           []string
	availReadLocations            []string
	availWriteEndpointsByLocation map[string]url.URL
	availReadEndpointsByLocation  map[string]url.URL
	writeEndpoints                []url.URL
	readEndpoints                 []url.URL
}

type accountRegion struct {
	Name     string `json:"name"`
	Endpoint string `json:"databaseAccountEndpoint"`
}

type userConsistencyPolicy struct {
	DefaultConsistencyLevel string `json:"defaultConsistencyLevel"`
}

type accountProperties struct {
	ReadRegions                  []accountRegion       `json:"readableLocations"`
	WriteRegions                 []accountRegion       `json:"writableLocations"`
	EnableMultipleWriteLocations bool                  `json:"enableMultipleWriteLocations"`
	AccountConsistency           userConsistencyPolicy `json:"userConsistencyPolicy"`
}

func (accountProps accountProperties) String() string {
	return fmt.Sprintf("Read regions: %v\nWrite regions: %v\nMulti-region writes: %v\nAccount consistency level: %v",
		accountProps.ReadRegions, accountProps.WriteRegions, accountProps.EnableMultipleWriteLocations, accountProps.AccountConsistency.DefaultConsistencyLevel)
}

type locationCache struct {
	locationInfo                      databaseAccountLocationsInfo
	defaultEndpoint                   url.URL
	enableCrossRegionRetries          bool
	locationUnavailabilityInfoMap     map[url.URL]locationUnavailabilityInfo
	mapMutex                          sync.RWMutex
	lastUpdateTime                    time.Time
	enableMultipleWriteLocations      bool
	unavailableLocationExpirationTime time.Duration
}

func newLocationCache(prefLocations []string, defaultEndpoint url.URL, enableCrossRegionRetries bool) *locationCache {
	return &locationCache{
		defaultEndpoint:                   defaultEndpoint,
		locationInfo:                      *newDatabaseAccountLocationsInfo(prefLocations, defaultEndpoint),
		locationUnavailabilityInfoMap:     make(map[url.URL]locationUnavailabilityInfo),
		unavailableLocationExpirationTime: defaultExpirationTime,
		enableCrossRegionRetries:          enableCrossRegionRetries,
	}
}

func (lc *locationCache) update(writeLocations []accountRegion, readLocations []accountRegion, prefList []string, enableMultipleWriteLocations *bool) error {
	nextLoc := copyDatabaseAccountLocationsInfo(lc.locationInfo)
	if prefList != nil {
		nextLoc.prefLocations = prefList
	}
	if enableMultipleWriteLocations != nil {
		lc.enableMultipleWriteLocations = *enableMultipleWriteLocations
	}
	lc.refreshStaleEndpoints()
	if readLocations != nil {
		availReadEndpointsByLocation, availReadLocations, err := getEndpointsByLocation(readLocations)
		if err != nil {
			return err
		}
		nextLoc.availReadEndpointsByLocation = availReadEndpointsByLocation
		nextLoc.availReadLocations = availReadLocations
		log.Write(azlog.EventResponse, fmt.Sprintf("\n===== Updated Read Locations =====\nLocations: %v\nEndpoints: %v\n=====\n",
			availReadLocations, availReadEndpointsByLocation))
	}

	if writeLocations != nil {
		availWriteEndpointsByLocation, availWriteLocations, err := getEndpointsByLocation(writeLocations)
		if err != nil {
			return err
		}
		nextLoc.availWriteEndpointsByLocation = availWriteEndpointsByLocation
		nextLoc.availWriteLocations = availWriteLocations
		log.Write(azlog.EventResponse, fmt.Sprintf("\n===== Updated Write Locations =====\nLocations: %v\nEndpoints: %v\n=====\n",
			availWriteLocations, availWriteEndpointsByLocation))
	}

	nextLoc.writeEndpoints = lc.getPrefAvailableEndpoints(nextLoc.availWriteEndpointsByLocation, nextLoc.availWriteLocations, write, lc.defaultEndpoint)
	nextLoc.readEndpoints = lc.getPrefAvailableEndpoints(nextLoc.availReadEndpointsByLocation, nextLoc.availReadLocations, read, nextLoc.writeEndpoints[0])
	lc.lastUpdateTime = time.Now()
	lc.locationInfo = nextLoc
	log.Write(azlog.EventResponse, fmt.Sprintf("\n===== Location Cache Updated =====\nPreferred Locations: %v\nWrite Endpoints (ordered): %v\nRead Endpoints (ordered): %v\nMulti-Master Enabled: %v\nDefault Endpoint: %s\n=====\n",
		nextLoc.prefLocations, nextLoc.writeEndpoints, nextLoc.readEndpoints, lc.enableMultipleWriteLocations, lc.defaultEndpoint.String()))
	return nil
}

func (lc *locationCache) resolveServiceEndpoint(locationIndex int, resourceType resourceType, isWriteOperation, useWriteEndpoint bool) url.URL {
	log.Write(azlog.EventRequest, fmt.Sprintf("\n===== Resolving Service Endpoint =====\nLocationIndex: %d\nResourceType: %v\nIsWriteOperation: %v\nUseWriteEndpoint: %v\nCanUseMultipleWriteLocs: %v\n",
		locationIndex, resourceType, isWriteOperation, useWriteEndpoint, lc.canUseMultipleWriteLocsToRoute(resourceType)))

	if (isWriteOperation || useWriteEndpoint) && !lc.canUseMultipleWriteLocsToRoute(resourceType) {
		if lc.enableCrossRegionRetries && len(lc.locationInfo.availWriteLocations) > 0 {
			locationIndex = min(locationIndex%2, len(lc.locationInfo.availWriteLocations)-1)
			writeLocation := lc.locationInfo.availWriteLocations[locationIndex]
			endpoint := lc.locationInfo.availWriteEndpointsByLocation[writeLocation]
			log.Write(azlog.EventRequest, fmt.Sprintf("===== Using Single Write Location =====\nWrite Location: %s\nEndpoint: %s\n=====\n",
				writeLocation, endpoint.String()))
			return endpoint
		}
		log.Write(azlog.EventRequest, fmt.Sprintf("===== Using Default Endpoint =====\nEndpoint: %s\n=====\n", lc.defaultEndpoint.String()))
		return lc.defaultEndpoint
	}

	endpoints := lc.locationInfo.readEndpoints
	if isWriteOperation {
		endpoints = lc.locationInfo.writeEndpoints
	}
	selectedIndex := locationIndex % len(endpoints)
	selectedEndpoint := endpoints[selectedIndex]
	log.Write(azlog.EventRequest, fmt.Sprintf("===== Selected Endpoint from List =====\nOperation: %s\nAvailable Endpoints: %v\nSelected Index: %d\nSelected Endpoint: %s\n=====\n",
		map[bool]string{true: "WRITE", false: "READ"}[isWriteOperation], endpoints, selectedIndex, selectedEndpoint.String()))
	return selectedEndpoint
}

func (lc *locationCache) canUseMultipleWriteLocsToRoute(resourceType resourceType) bool {
	return lc.canUseMultipleWriteLocs() && resourceType == resourceTypeDocument
}

func (lc *locationCache) readEndpoints() ([]url.URL, error) {
	lc.mapMutex.RLock()
	defer lc.mapMutex.RUnlock()
	if time.Since(lc.lastUpdateTime) > lc.unavailableLocationExpirationTime && len(lc.locationUnavailabilityInfoMap) > 0 {
		err := lc.update(nil, nil, nil, nil)
		if err != nil {
			return nil, err
		}
	}
	return lc.locationInfo.readEndpoints, nil
}

func (lc *locationCache) writeEndpoints() ([]url.URL, error) {
	lc.mapMutex.RLock()
	defer lc.mapMutex.RUnlock()
	if time.Since(lc.lastUpdateTime) > lc.unavailableLocationExpirationTime && len(lc.locationUnavailabilityInfoMap) > 0 {
		err := lc.update(nil, nil, nil, nil)
		if err != nil {
			return nil, err
		}
	}
	return lc.locationInfo.writeEndpoints, nil
}

func (lc *locationCache) getLocation(endpoint url.URL) string {
	firstLoc := ""
	for location, uri := range lc.locationInfo.availWriteEndpointsByLocation {
		if uri == endpoint {
			return location
		}
		if firstLoc == "" {
			firstLoc = location
		}
	}

	for location, uri := range lc.locationInfo.availReadEndpointsByLocation {
		if uri == endpoint {
			return location
		}
	}

	if endpoint == lc.defaultEndpoint && !lc.canUseMultipleWriteLocs() {
		if len(lc.locationInfo.availWriteEndpointsByLocation) > 0 {
			return firstLoc
		}
	}
	return ""
}

func (lc *locationCache) canUseMultipleWriteLocs() bool {
	return lc.enableMultipleWriteLocations
}

func (lc *locationCache) markEndpointUnavailableForRead(endpoint url.URL) error {
	return lc.markEndpointUnavailable(endpoint, read)
}

func (lc *locationCache) markEndpointUnavailableForWrite(endpoint url.URL) error {
	return lc.markEndpointUnavailable(endpoint, write)
}

func (lc *locationCache) markEndpointUnavailable(endpoint url.URL, op requestedOperations) error {
	now := time.Now()
	lc.mapMutex.Lock()
	if info, ok := lc.locationUnavailabilityInfoMap[endpoint]; ok {
		info.lastCheckTime = now
		info.unavailableOps |= op
		lc.locationUnavailabilityInfoMap[endpoint] = info
		log.Write(azlog.EventRetryPolicy, fmt.Sprintf("\n===== Endpoint Marked Unavailable (Updated) =====\nEndpoint: %s\nOperation: %v\nCombined Unavailable Ops: %v\n=====\n",
			endpoint.String(), op, info.unavailableOps))
	} else {
		info = locationUnavailabilityInfo{
			lastCheckTime:  now,
			unavailableOps: op,
		}
		lc.locationUnavailabilityInfoMap[endpoint] = info
		log.Write(azlog.EventRetryPolicy, fmt.Sprintf("\n===== Endpoint Marked Unavailable (New) =====\nEndpoint: %s\nOperation: %v\n=====\n",
			endpoint.String(), op))
	}
	lc.mapMutex.Unlock()
	err := lc.update(nil, nil, nil, nil)
	return err
}

func (lc *locationCache) databaseAccountRead(dbAcct accountProperties) error {
	return lc.update(dbAcct.WriteRegions, dbAcct.ReadRegions, nil, &dbAcct.EnableMultipleWriteLocations)
}

func (lc *locationCache) refreshStaleEndpoints() {
	lc.mapMutex.Lock()
	defer lc.mapMutex.Unlock()
	for endpoint, info := range lc.locationUnavailabilityInfoMap {
		t := time.Since(info.lastCheckTime)
		if t > lc.unavailableLocationExpirationTime {
			delete(lc.locationUnavailabilityInfoMap, endpoint)
		}
	}
}

func (lc *locationCache) isEndpointUnavailable(endpoint url.URL, ops requestedOperations) bool {
	lc.mapMutex.RLock()
	info, ok := lc.locationUnavailabilityInfoMap[endpoint]
	lc.mapMutex.RUnlock()
	if ops == none || !ok || ops&info.unavailableOps != ops {
		return false
	}
	return time.Since(info.lastCheckTime) < lc.unavailableLocationExpirationTime
}

func (lc *locationCache) getPrefAvailableEndpoints(endpointsByLoc map[string]url.URL, locs []string, availOps requestedOperations, fallbackEndpoint url.URL) []url.URL {
	endpoints := make([]url.URL, 0)
	log.Write(azlog.EventResponse, fmt.Sprintf("\n===== Building Preferred Available Endpoints =====\nPreferred Locations: %v\nAvailable Locations: %v\nOperation Type: %v\nFallback Endpoint: %s\nCross-Region Retries Enabled: %v\n",
		lc.locationInfo.prefLocations, locs, availOps, fallbackEndpoint.String(), lc.enableCrossRegionRetries))

	if lc.enableCrossRegionRetries {
		if lc.canUseMultipleWriteLocs() || availOps&read != 0 {
			unavailEndpoints := make([]url.URL, 0)
			unavailEndpoints = append(unavailEndpoints, fallbackEndpoint)
			log.Write(azlog.EventResponse, fmt.Sprintf("===== Fallback endpoint added to unavailable list: %s\n", fallbackEndpoint.String()))

			for _, loc := range lc.locationInfo.prefLocations {
				if endpoint, ok := endpointsByLoc[loc]; ok && endpoint != fallbackEndpoint {
					if lc.isEndpointUnavailable(endpoint, availOps) {
						unavailEndpoints = append(unavailEndpoints, endpoint)
						log.Write(azlog.EventResponse, fmt.Sprintf("===== Location '%s' endpoint %s is UNAVAILABLE, added to unavailable list\n", loc, endpoint.String()))
					} else {
						endpoints = append(endpoints, endpoint)
						log.Write(azlog.EventResponse, fmt.Sprintf("===== Location '%s' endpoint %s is AVAILABLE, added to available list\n", loc, endpoint.String()))
					}
				} else if !ok {
					log.Write(azlog.EventResponse, fmt.Sprintf("===== Location '%s' not found in endpoints map\n", loc))
				} else {
					log.Write(azlog.EventResponse, fmt.Sprintf("===== Location '%s' endpoint matches fallback, skipping\n", loc))
				}
			}
			endpoints = append(endpoints, unavailEndpoints...)
			log.Write(azlog.EventResponse, fmt.Sprintf("===== Final Endpoint Order =====\nAvailable Endpoints (preferred): %v\nUnavailable Endpoints (fallback): %v\nCombined Order: %v\n=====\n",
				endpoints[:len(endpoints)-len(unavailEndpoints)], unavailEndpoints, endpoints))
		} else {
			for _, loc := range locs {
				if endpoint, ok := endpointsByLoc[loc]; ok && loc != "" {
					endpoints = append(endpoints, endpoint)
				}
			}
			log.Write(azlog.EventResponse, fmt.Sprintf("===== Using available locations (no multi-write) =====\nEndpoints: %v\n=====\n", endpoints))
		}
	}
	if len(endpoints) == 0 {
		endpoints = append(endpoints, fallbackEndpoint)
		log.Write(azlog.EventResponse, fmt.Sprintf("===== No endpoints found, using fallback =====\nFallback: %s\n=====\n", fallbackEndpoint.String()))
	}
	return endpoints
}

func getEndpointsByLocation(locs []accountRegion) (map[string]url.URL, []string, error) {
	endpointsByLoc := make(map[string]url.URL)
	parsedLocs := make([]string, 0)
	for _, loc := range locs {
		endpoint, err := url.Parse(loc.Endpoint)
		if err != nil {
			return nil, nil, err
		}
		if loc.Name != "" {
			endpointsByLoc[loc.Name] = *endpoint
			parsedLocs = append(parsedLocs, loc.Name)
		}
		// TODO else: log
	}
	return endpointsByLoc, parsedLocs, nil
}

func newDatabaseAccountLocationsInfo(prefLocations []string, defaultEndpoint url.URL) *databaseAccountLocationsInfo {
	availWriteLocs := make([]string, 0)
	availReadLocs := make([]string, 0)
	availWriteEndpointsByLocation := make(map[string]url.URL)
	availReadEndpointsByLocation := make(map[string]url.URL)
	writeEndpoints := []url.URL{defaultEndpoint}
	readEndpoints := []url.URL{defaultEndpoint}
	return &databaseAccountLocationsInfo{
		prefLocations:                 prefLocations,
		availWriteLocations:           availWriteLocs,
		availReadLocations:            availReadLocs,
		availWriteEndpointsByLocation: availWriteEndpointsByLocation,
		availReadEndpointsByLocation:  availReadEndpointsByLocation,
		writeEndpoints:                writeEndpoints,
		readEndpoints:                 readEndpoints,
	}
}

func copyDatabaseAccountLocationsInfo(other databaseAccountLocationsInfo) databaseAccountLocationsInfo {
	return databaseAccountLocationsInfo{
		prefLocations:                 other.prefLocations,
		availWriteLocations:           other.availWriteLocations,
		availReadLocations:            other.availReadLocations,
		availWriteEndpointsByLocation: other.availWriteEndpointsByLocation,
		availReadEndpointsByLocation:  other.availReadEndpointsByLocation,
		writeEndpoints:                other.writeEndpoints,
		readEndpoints:                 other.readEndpoints,
	}
}
