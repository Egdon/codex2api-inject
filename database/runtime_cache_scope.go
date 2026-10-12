package database

import "context"

func (db *DB) initializeRuntimeCacheScope(ctx context.Context) error {
	revision, err := db.GetAPIKeyAuthRevision(ctx)
	if err != nil {
		return err
	}
	db.runtimeCacheScope = revision.Namespace
	return nil
}

// RuntimeCacheScope 隔离使用同一个 Redis 的不同数据库，不包含连接凭证原文。
func (db *DB) RuntimeCacheScope() string {
	if db == nil {
		return ""
	}
	return db.runtimeCacheScope
}
