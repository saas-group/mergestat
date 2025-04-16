package cron

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mergestat/mergestat/internal/jobs/sync/podman"
	"github.com/mergestat/sqlq"
	"github.com/rs/zerolog"
)

// ContainerSync provides a cron function that periodically schedules execution
// of configured mergestat.container_sync_schedules.
func ContainerSync(ctx context.Context, dur time.Duration, upstream *sql.DB, delay time.Duration) {
	var log = zerolog.Ctx(ctx)

	type Sync = struct {
		ID          uuid.UUID
		Queue       sqlq.Queue
		Concurrency int32
		Priority    int32
	}

	const listSyncsQuery = `
WITH schedules(id, queue, job, status, concurrency, priority, last_completed_at) AS (
	SELECT DISTINCT ON (syncs.id) syncs.id, (image.queue || '-' || repo.provider) AS queue, exec.job_id, job.status,
		CASE WHEN image.queue = 'github' THEN 1 ELSE 0 END AS concurrency,
		CASE WHEN image.queue = 'github' THEN 1 ELSE 2 END AS priority,
		job.completed_at
		FROM mergestat.container_sync_schedules schd, mergestat.container_syncs syncs
			INNER JOIN mergestat.container_images image ON image.id = syncs.image_id
			INNER JOIN public.repos repo ON repo.id = syncs.repo_id
			LEFT OUTER JOIN mergestat.container_sync_executions exec ON exec.sync_id = syncs.id
			LEFT OUTER JOIN sqlq.jobs job ON job.id = exec.job_id
	WHERE syncs.id = schd.sync_id
	ORDER BY syncs.id, exec.created_at DESC
)
SELECT id, queue, concurrency, priority FROM schedules
	WHERE (status IS NULL OR status NOT IN ('pending', 'running'))
	AND (last_completed_at IS NULL OR last_completed_at < now() - $1::interval);`

	const createExecutionQuery = "INSERT INTO mergestat.container_sync_executions (sync_id, job_id) VALUES ($1, $2)"
	const createQueueQuery = "INSERT INTO sqlq.queues (name, concurrency, priority) VALUES ($1, NULLIF($2,0), $3) ON CONFLICT (name) DO UPDATE SET concurrency = excluded.concurrency, priority = excluded.priority"

	var fn = func() error {
		var err error
		var tx *sql.Tx
		if tx, err = upstream.BeginTx(ctx, &sql.TxOptions{}); err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck

		var syncs []Sync
		var rows *sql.Rows
		if rows, err = tx.QueryContext(ctx, listSyncsQuery, fmt.Sprintf("%d minutes", int64(delay.Minutes()))); err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var sync Sync
			if err = rows.Scan(&sync.ID, &sync.Queue, &sync.Concurrency, &sync.Priority); err != nil {
				return err
			}
			syncs = append(syncs, sync)
		}

		var createQueue *sql.Stmt
		if createQueue, err = tx.PrepareContext(ctx, createQueueQuery); err != nil {
			return err
		}
		defer createQueue.Close()

		var createExecution *sql.Stmt
		if createExecution, err = tx.PrepareContext(ctx, createExecutionQuery); err != nil {
			return err
		}
		defer createExecution.Close()

		for _, sync := range syncs {
			if _, err = createQueue.ExecContext(ctx, sync.Queue, sync.Concurrency, sync.Priority); err != nil {
				return err
			}

			var job *sqlq.Job
			if job, err = sqlq.Enqueue(tx, sync.Queue, podman.NewContainerSync(sync.ID)); err != nil {
				return err
			}

			if _, err = createExecution.ExecContext(ctx, sync.ID, job.ID); err != nil {
				return err
			}
		}

		return tx.Commit()
	}

	// reuse existing loop-select functionality in Basic()
	Basic(ctx, dur, func() {
		if err := fn(); err != nil {
			log.Err(err).Msg("failed to start container sync")
		}
	})
}
