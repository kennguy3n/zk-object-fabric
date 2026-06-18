package main

import (
	"log"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/internal/notifications"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

// notificationEmitter adapts the async notifications.Dispatcher to the
// s3compat.NotificationEmitter interface, converting the handler's
// transport-agnostic ObjectEvent into a notifications.Event. It is the
// thin shim that keeps api/s3compat free of an internal/notifications
// import (mirroring how the audit and billing sinks are wired). A nil
// emitter (when notifications are disabled) is never installed: the
// handler's Config.Notifications stays nil and notify() short-circuits.
type notificationEmitter struct {
	dispatcher *notifications.Dispatcher
}

func (e notificationEmitter) Emit(evt s3compat.ObjectEvent) {
	e.dispatcher.Notify(notifications.Event{
		TenantID:  evt.TenantID,
		Bucket:    evt.Bucket,
		Name:      notification.EventType(evt.EventName),
		ObjectKey: evt.ObjectKey,
		SizeBytes: evt.SizeBytes,
		ETag:      evt.ETag,
		VersionID: evt.VersionID,
		RequestID: evt.RequestID,
		SourceIP:  evt.SourceIP,
	})
}

// buildNotificationDispatcher constructs the bucket event-notification
// dispatcher when enabled in config and a bucket-config store
// is available to resolve per-bucket rules. It returns (nil, nil) when
// notifications are disabled or unsupported, in which case the s3
// handler is wired with no emitter and events are never delivered (the
// configuration sub-resource still works). The caller owns the
// returned dispatcher and must Close it on shutdown.
func buildNotificationDispatcher(cfg config.NotificationsConfig, store bucket_config.Store) (*notifications.Dispatcher, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if store == nil {
		// No backing store means no rules can ever be resolved;
		// delivering nothing is the only coherent behaviour, so skip
		// the dispatcher entirely rather than spin idle workers.
		log.Printf("notifications: enabled but no bucket-config store is configured; event delivery disabled")
		return nil, nil
	}
	return notifications.New(notifications.Config{
		Source:                   store,
		Workers:                  cfg.Workers,
		QueueSize:                cfg.QueueSize,
		MaxAttempts:              cfg.MaxAttempts,
		BackoffBase:              cfg.BackoffBase.ToDuration(),
		DeliveryTimeout:          cfg.DeliveryTimeout.ToDuration(),
		ShutdownGrace:            cfg.ShutdownGrace.ToDuration(),
		AllowPrivateDestinations: cfg.AllowPrivateDestinations,
	})
}
