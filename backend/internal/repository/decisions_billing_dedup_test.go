//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDecisionsBillingDuplicateDoesNotApplyMoneyEffects(t *testing.T) {
	for _, fingerprint := range []string{"same", "different"} {
		t.Run(fingerprint, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			mock.ExpectBegin()
			mock.ExpectQuery("INSERT INTO usage_billing_dedup").WithArgs("dec-request", int64(2), "same").WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery("SELECT request_fingerprint").WithArgs("dec-request", int64(2)).WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(fingerprint))
			mock.ExpectRollback()
			repo := &usageBillingRepository{db: db}
			result, err := repo.Apply(context.Background(), &service.UsageBillingCommand{RequestID: "dec-request", APIKeyID: 2, RequestFingerprint: "same", Model: "gpt-6-luna", UserID: 1, BalanceCost: 0.000169})
			if fingerprint == "different" {
				require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
			} else {
				require.NoError(t, err)
				require.False(t, result.Applied)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
