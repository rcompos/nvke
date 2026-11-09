// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Empty values keep the kubelet patch's 1000m default.
		for _, table := range []string{"node_pools", "node_sets"} {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE "+table+" ADD COLUMN IF NOT EXISTS kube_reserved_cpu text NOT NULL DEFAULT ''",
			); err != nil {
				return fmt.Errorf("add %s.kube_reserved_cpu: %w", table, err)
			}
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
