CREATE TABLE "subscriptions" (`id` integer,`relay_url` text,`webhook_url` text,`ids_string` text,`kinds_string` text,`authors_string` text,`tags_string` text,`since` datetime,`until` datetime,`limit` integer,`search` text,`open` boolean,`created_at` datetime,`updated_at` datetime,PRIMARY KEY (`id`));
CREATE TABLE "request_events" (`id` integer,`subscription_id` integer,`nostr_id` text UNIQUE,`content` text,`state` text,`created_at` datetime,`updated_at` datetime,PRIMARY KEY (`id`),CONSTRAINT `fk_request_events_subscription` FOREIGN KEY (`subscription_id`) REFERENCES `subscriptions`(`id`) ON DELETE CASCADE);
CREATE TABLE "response_events" (`id` integer,`subscription_id` integer,`request_id` integer null,`nostr_id` text,`content` text,`replied_at` datetime,`created_at` datetime,`updated_at` datetime,PRIMARY KEY (`id`),CONSTRAINT `fk_response_events_subscription` FOREIGN KEY (`subscription_id`) REFERENCES `subscriptions`(`id`) ON DELETE CASCADE,CONSTRAINT `fk_response_events_request_event` FOREIGN KEY (`request_id`) REFERENCES `request_events`(`id`) ON DELETE CASCADE);