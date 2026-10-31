// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/go-acme/lego/v5/challenge"
	legodns "github.com/go-acme/lego/v5/providers/dns"
)

var dnsProviderEnvironmentMu sync.Mutex

func newDNS01Provider(name string, parameters map[string]string) (challenge.Provider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil, errors.New("DNS-01 provider name is required")
	}

	dnsProviderEnvironmentMu.Lock()
	defer dnsProviderEnvironmentMu.Unlock()

	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type previousValue struct {
		value string
		set   bool
	}
	previous := make(map[string]previousValue, len(parameters))
	applied := make([]string, 0, len(parameters))
	restore := func() error {
		var restoreErr error
		for i := len(applied) - 1; i >= 0; i-- {
			key := applied[i]
			value := previous[key]
			var err error
			if value.set {
				err = os.Setenv(key, value.value)
			} else {
				err = os.Unsetenv(key)
			}
			if err != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore DNS-01 provider parameter %q: %w", key, err))
			}
		}
		return restoreErr
	}

	for _, key := range keys {
		value, set := os.LookupEnv(key)
		previous[key] = previousValue{value: value, set: set}
		if err := os.Setenv(key, parameters[key]); err != nil {
			return nil, errors.Join(fmt.Errorf("set DNS-01 provider parameter %q: %w", key, err), restore())
		}
		applied = append(applied, key)
	}
	provider, providerErr := legodns.NewDNSChallengeProviderByName(name)
	if providerErr != nil {
		return nil, errors.Join(fmt.Errorf("create DNS-01 provider %q: %w", name, providerErr), restore())
	}
	if err := restore(); err != nil {
		return nil, err
	}
	return provider, nil
}
