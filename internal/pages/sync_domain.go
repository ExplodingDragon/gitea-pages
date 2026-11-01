// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitea.dev/page/internal/gitea"
)

const txtAuthorizationGracePeriod = 24 * time.Hour

func (d *deployer) customDomainAuthorized(ctx context.Context, site gitea.Site, requireGiteaTXT bool) (bool, error) {
	if site.CustomDomain == nil {
		return false, nil
	}
	domain := site.CustomDomain
	if !requireGiteaTXT {
		return true, nil
	}
	now := time.Now()
	lastGiteaSuccess := time.Unix(0, domain.TXTLastSuccessUnixNano)
	withinGiteaGrace := domain.TXTLastSuccessUnixNano != 0 && !lastGiteaSuccess.After(now) && now.Sub(lastGiteaSuccess) < txtAuthorizationGracePeriod
	if domain.TXTResult != gitea.DomainTXTValid && !withinGiteaGrace {
		return false, fmt.Errorf("domain %s is not authorized by Gitea", domain.Host)
	}
	key := fmt.Sprintf("%d/%s/%s", domain.ID, domain.Host, domain.TXTToken)
	local, present := d.store.TXTState(key)
	lastCheck := time.Unix(0, local.LastCheckUnixNano)
	lastLocalSuccess := time.Unix(0, local.LastSuccessUnixNano)
	localValid := local.LastSuccessUnixNano != 0 && !lastLocalSuccess.After(now) && now.Sub(lastLocalSuccess) < txtAuthorizationGracePeriod
	if present && !lastCheck.After(now) && now.Sub(lastCheck) < d.cfg.DNSRefreshInterval {
		if localValid {
			return true, nil
		}
		return false, fmt.Errorf("domain %s has no current local TXT authorization", domain.Host)
	}
	result, err := verifyTXTResult(ctx, d.resolver, domain.Host, domain.TXTToken)
	local.LastCheckUnixNano = now.UnixNano()
	switch result {
	case txtValid:
		local.LastSuccessUnixNano = now.UnixNano()
		localValid = true
	case txtInvalid, txtTemporaryError:
		// A failed refresh keeps a recent authorization until the grace period expires.
	default:
		return false, errors.New("unknown TXT verification result")
	}
	if saveErr := d.store.SetTXTState(ctx, key, local); saveErr != nil {
		return false, saveErr
	}
	if !localValid {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("domain %s has no current local TXT authorization", domain.Host)
	}
	return true, nil
}
