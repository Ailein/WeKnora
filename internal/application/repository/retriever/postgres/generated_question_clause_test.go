package postgres

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pg_search 0.25 fails a keyword query whose WHERE has NOT over the
// generated-question predicate with "Unsupported query shape"; the exclusion
// must stay in the IS NOT TRUE form it accepts.
func TestKeywordsRetrieveGeneratedQuestionExclusionShape(t *testing.T) {
	var queries []string
	recordAll := sqlmock.QueryMatcherFunc(func(_, actual string) error {
		queries = append(queries, actual)
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(recordAll))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); _ = db.Close() })
	gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: db}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	repo := NewPostgresRetrieveEngineRepository(gdb).(*pgRepository)

	mock.ExpectQuery("score function probe").
		WillReturnRows(sqlmock.NewRows([]string{"score_function"}).AddRow("paradedb.score"))
	mock.ExpectQuery("keyword retrieval").
		WithArgs("kb-off", "pizza", true, "kb-off", 5).
		WillReturnRows(sqlmock.NewRows([]string{"id", "score"}))

	_, err = repo.KeywordsRetrieve(context.Background(), types.RetrieveParams{
		Query: "pizza", TopK: 5,
		KnowledgeBaseIDs:              []string{"kb-off"},
		ExcludeGeneratedQuestionKBIDs: []string{"kb-off"},
	})
	require.NoError(t, err)

	require.Len(t, queries, 2)
	keywordSQL := queries[1]
	require.Contains(t, keywordSQL,
		"(knowledge_base_id IN ($4) AND "+types.GeneratedQuestionRowPredicate("")+") IS NOT TRUE")
	require.NotContains(t, keywordSQL, "NOT (")
}
