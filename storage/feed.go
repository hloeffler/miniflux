// Copyright 2017 Frédéric Guillot. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package storage // import "miniflux.app/storage"

import (
	"database/sql"
	"errors"
	"fmt"

	"miniflux.app/model"
	"miniflux.app/timezone"
)

var feedListQuery = `
	SELECT
		f.id,
		f.feed_url,
		f.site_url,
		f.title,
		f.etag_header,
		f.last_modified_header,
		f.user_id,
		f.checked_at at time zone u.timezone,
		f.parsing_error_count,
		f.parsing_error_msg,
		f.scraper_rules,
		f.rewrite_rules,
		f.crawler,
		f.user_agent,
		f.username,
		f.password,
		f.ignore_http_cache,
		f.fetch_via_proxy,
		f.disabled,
		f.category_id,
		c.title as category_title,
		fi.icon_id,
		u.timezone
	FROM
		feeds f
	LEFT JOIN
		categories c ON c.id=f.category_id
	LEFT JOIN
		feed_icons fi ON fi.feed_id=f.id
	LEFT JOIN
		users u ON u.id=f.user_id
	WHERE
		f.user_id=$1
	ORDER BY
		f.parsing_error_count DESC, lower(f.title) ASC
`

// FeedExists checks if the given feed exists.
func (s *Storage) FeedExists(userID, feedID int64) bool {
	var result bool
	query := `SELECT true FROM feeds WHERE user_id=$1 AND id=$2`
	s.db.QueryRow(query, userID, feedID).Scan(&result)
	return result
}

// FeedURLExists checks if feed URL already exists.
func (s *Storage) FeedURLExists(userID int64, feedURL string) bool {
	var result bool
	query := `SELECT true FROM feeds WHERE user_id=$1 AND feed_url=$2`
	s.db.QueryRow(query, userID, feedURL).Scan(&result)
	return result
}

// AnotherFeedURLExists checks if the user a duplicated feed.
func (s *Storage) AnotherFeedURLExists(userID, feedID int64, feedURL string) bool {
	var result bool
	query := `SELECT true FROM feeds WHERE id <> $1 AND user_id=$2 AND feed_url=$3`
	s.db.QueryRow(query, feedID, userID, feedURL).Scan(&result)
	return result
}

// CountAllFeeds returns the number of feeds in the database.
func (s *Storage) CountAllFeeds() map[string]int64 {
	rows, err := s.db.Query(`SELECT disabled, count(*) FROM feeds GROUP BY disabled`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	results := make(map[string]int64)
	results["enabled"] = 0
	results["disabled"] = 0

	for rows.Next() {
		var disabled bool
		var count int64

		if err := rows.Scan(&disabled, &count); err != nil {
			continue
		}

		if disabled {
			results["disabled"] = count
		} else {
			results["enabled"] = count
		}
	}

	results["total"] = results["disabled"] + results["enabled"]
	return results
}

// CountFeeds returns the number of feeds that belongs to the given user.
func (s *Storage) CountFeeds(userID int64) int {
	var result int
	err := s.db.QueryRow(`SELECT count(*) FROM feeds WHERE user_id=$1`, userID).Scan(&result)
	if err != nil {
		return 0
	}

	return result
}

// CountUserFeedsWithErrors returns the number of feeds with parsing errors that belong to the given user.
func (s *Storage) CountUserFeedsWithErrors(userID int64) int {
	query := `SELECT count(*) FROM feeds WHERE user_id=$1 AND parsing_error_count >= $2`
	var result int
	err := s.db.QueryRow(query, userID, maxParsingError).Scan(&result)
	if err != nil {
		return 0
	}

	return result
}

// CountAllFeedsWithErrors returns the number of feeds with parsing errors.
func (s *Storage) CountAllFeedsWithErrors() int {
	query := `SELECT count(*) FROM feeds WHERE parsing_error_count >= $1`
	var result int
	err := s.db.QueryRow(query, maxParsingError).Scan(&result)
	if err != nil {
		return 0
	}

	return result
}

// Feeds returns all feeds that belongs to the given user.
func (s *Storage) Feeds(userID int64) (model.Feeds, error) {
	return s.fetchFeeds(feedListQuery, "", userID)
}

// FeedsWithCounters returns all feeds of the given user with counters of read and unread entries.
func (s *Storage) FeedsWithCounters(userID int64) (model.Feeds, error) {
	counterQuery := `
		SELECT
			feed_id,
			status,
			count(*)
		FROM
			entries
		WHERE
			user_id=$1 AND status IN ('read', 'unread')
		GROUP BY
			feed_id, status
	`
	return s.fetchFeeds(feedListQuery, counterQuery, userID)
}

// FeedsByCategoryWithCounters returns all feeds of the given user/category with counters of read and unread entries.
func (s *Storage) FeedsByCategoryWithCounters(userID, categoryID int64) (model.Feeds, error) {
	feedQuery := `
		SELECT
			f.id,
			f.feed_url,
			f.site_url,
			f.title,
			f.etag_header,
			f.last_modified_header,
			f.user_id,
			f.checked_at at time zone u.timezone,
			f.parsing_error_count,
			f.parsing_error_msg,
			f.scraper_rules,
			f.rewrite_rules,
			f.crawler,
			f.user_agent,
			f.username,
			f.password,
			f.ignore_http_cache,
			f.fetch_via_proxy,
			f.disabled,
			f.category_id,
			c.title as category_title,
			fi.icon_id,
			u.timezone
		FROM
			feeds f
		LEFT JOIN
			categories c ON c.id=f.category_id
		LEFT JOIN
			feed_icons fi ON fi.feed_id=f.id
		LEFT JOIN
			users u ON u.id=f.user_id
		WHERE
			f.user_id=$1 AND f.category_id=$2
		ORDER BY
			f.parsing_error_count DESC, lower(f.title) ASC
	`

	counterQuery := `
		SELECT
			e.feed_id,
			e.status,
			count(*)
		FROM
			entries e
		LEFT JOIN
			feeds f ON f.id=e.feed_id
		WHERE
			e.user_id=$1 AND f.category_id=$2 AND e.status IN ('read', 'unread')
		GROUP BY
			e.feed_id, e.status
	`

	return s.fetchFeeds(feedQuery, counterQuery, userID, categoryID)
}

func (s *Storage) fetchFeedCounter(query string, args ...interface{}) (unreadCounters map[int64]int, readCounters map[int64]int, err error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf(`store: unable to fetch feed counts: %v`, err)
	}
	defer rows.Close()

	readCounters = make(map[int64]int)
	unreadCounters = make(map[int64]int)
	for rows.Next() {
		var feedID int64
		var status string
		var count int
		if err := rows.Scan(&feedID, &status, &count); err != nil {
			return nil, nil, fmt.Errorf(`store: unable to fetch feed counter row: %v`, err)
		}

		if status == "read" {
			readCounters[feedID] = count
		} else if status == "unread" {
			unreadCounters[feedID] = count
		}
	}

	return readCounters, unreadCounters, nil
}

func (s *Storage) fetchFeeds(feedQuery, counterQuery string, args ...interface{}) (model.Feeds, error) {
	var (
		readCounters   map[int64]int
		unreadCounters map[int64]int
	)

	if counterQuery != "" {
		var err error
		readCounters, unreadCounters, err = s.fetchFeedCounter(counterQuery, args...)
		if err != nil {
			return nil, err
		}
	}

	feeds := make(model.Feeds, 0)
	rows, err := s.db.Query(feedQuery, args...)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch feeds: %v`, err)
	}
	defer rows.Close()

	for rows.Next() {
		var feed model.Feed
		var iconID interface{}
		var tz string
		feed.Category = &model.Category{}

		err := rows.Scan(
			&feed.ID,
			&feed.FeedURL,
			&feed.SiteURL,
			&feed.Title,
			&feed.EtagHeader,
			&feed.LastModifiedHeader,
			&feed.UserID,
			&feed.CheckedAt,
			&feed.ParsingErrorCount,
			&feed.ParsingErrorMsg,
			&feed.ScraperRules,
			&feed.RewriteRules,
			&feed.Crawler,
			&feed.UserAgent,
			&feed.Username,
			&feed.Password,
			&feed.IgnoreHTTPCache,
			&feed.FetchViaProxy,
			&feed.Disabled,
			&feed.Category.ID,
			&feed.Category.Title,
			&iconID,
			&tz,
		)

		if err != nil {
			return nil, fmt.Errorf(`store: unable to fetch feeds row: %v`, err)
		}

		if iconID != nil {
			feed.Icon = &model.FeedIcon{FeedID: feed.ID, IconID: iconID.(int64)}
		}

		if counterQuery != "" {
			if count, found := readCounters[feed.ID]; found {
				feed.ReadCount = count
			}

			if count, found := unreadCounters[feed.ID]; found {
				feed.UnreadCount = count
			}
		}

		feed.CheckedAt = timezone.Convert(tz, feed.CheckedAt)
		feed.Category.UserID = feed.UserID
		feeds = append(feeds, &feed)
	}

	return feeds, nil
}

// WeeklyFeedEntryCount returns the weekly entry count for a feed.
func (s *Storage) WeeklyFeedEntryCount(userID, feedID int64) (int, error) {
	query := `
		SELECT
			count(*)
		FROM
			entries
		WHERE
			entries.user_id=$1 AND 
			entries.feed_id=$2 AND 
			entries.published_at BETWEEN (now() - interval '1 week') AND now();
	`

	var weeklyCount int
	err := s.db.QueryRow(query, userID, feedID).Scan(&weeklyCount)

	switch {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf(`store: unable to fetch weekly count for feed #%d: %v`, feedID, err)
	}

	return weeklyCount, nil
}

// FeedByID returns a feed by the ID.
func (s *Storage) FeedByID(userID, feedID int64) (*model.Feed, error) {
	var feed model.Feed
	var iconID interface{}
	var tz string
	feed.Category = &model.Category{UserID: userID}

	query := `
		SELECT
			f.id,
			f.feed_url,
			f.site_url,
			f.title,
			f.etag_header,
			f.last_modified_header,
			f.user_id, f.checked_at at time zone u.timezone,
			f.parsing_error_count,
			f.parsing_error_msg,
			f.scraper_rules,
			f.rewrite_rules,
			f.crawler,
			f.user_agent,
			f.username,
			f.password,
			f.ignore_http_cache,
			f.fetch_via_proxy,
			f.disabled,
			f.category_id,
			c.title as category_title,
			fi.icon_id,
			u.timezone
		FROM feeds f
		LEFT JOIN categories c ON c.id=f.category_id
		LEFT JOIN feed_icons fi ON fi.feed_id=f.id
		LEFT JOIN users u ON u.id=f.user_id
		WHERE
			f.user_id=$1 AND f.id=$2
	`

	err := s.db.QueryRow(query, userID, feedID).Scan(
		&feed.ID,
		&feed.FeedURL,
		&feed.SiteURL,
		&feed.Title,
		&feed.EtagHeader,
		&feed.LastModifiedHeader,
		&feed.UserID,
		&feed.CheckedAt,
		&feed.ParsingErrorCount,
		&feed.ParsingErrorMsg,
		&feed.ScraperRules,
		&feed.RewriteRules,
		&feed.Crawler,
		&feed.UserAgent,
		&feed.Username,
		&feed.Password,
		&feed.IgnoreHTTPCache,
		&feed.FetchViaProxy,
		&feed.Disabled,
		&feed.Category.ID,
		&feed.Category.Title,
		&iconID,
		&tz,
	)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf(`store: unable to fetch feed #%d: %v`, feedID, err)
	}

	if iconID != nil {
		feed.Icon = &model.FeedIcon{FeedID: feed.ID, IconID: iconID.(int64)}
	}

	feed.CheckedAt = timezone.Convert(tz, feed.CheckedAt)
	return &feed, nil
}

// CreateFeed creates a new feed.
func (s *Storage) CreateFeed(feed *model.Feed) error {
	sql := `
		INSERT INTO feeds (
			feed_url,
			site_url,
			title,
			category_id,
			user_id,
			etag_header,
			last_modified_header,
			crawler,
			user_agent,
			username,
			password,
			disabled,
			scraper_rules,
			rewrite_rules,
			fetch_via_proxy
		)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING
			id
	`
	err := s.db.QueryRow(
		sql,
		feed.FeedURL,
		feed.SiteURL,
		feed.Title,
		feed.Category.ID,
		feed.UserID,
		feed.EtagHeader,
		feed.LastModifiedHeader,
		feed.Crawler,
		feed.UserAgent,
		feed.Username,
		feed.Password,
		feed.Disabled,
		feed.ScraperRules,
		feed.RewriteRules,
		feed.FetchViaProxy,
	).Scan(&feed.ID)
	if err != nil {
		return fmt.Errorf(`store: unable to create feed %q: %v`, feed.FeedURL, err)
	}

	for i := 0; i < len(feed.Entries); i++ {
		feed.Entries[i].FeedID = feed.ID
		feed.Entries[i].UserID = feed.UserID

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf(`store: unable to start transaction: %v`, err)
		}

		if !s.entryExists(tx, feed.Entries[i]) {
			if err := s.createEntry(tx, feed.Entries[i]); err != nil {
				tx.Rollback()
				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf(`store: unable to commit transaction: %v`, err)
		}
	}

	return nil
}

// UpdateFeed updates an existing feed.
func (s *Storage) UpdateFeed(feed *model.Feed) (err error) {
	query := `
		UPDATE
			feeds
		SET
			feed_url=$1,
			site_url=$2,
			title=$3,
			category_id=$4,
			etag_header=$5,
			last_modified_header=$6,
			checked_at=$7,
			parsing_error_msg=$8,
			parsing_error_count=$9,
			scraper_rules=$10,
			rewrite_rules=$11,
			crawler=$12,
			user_agent=$13,
			username=$14,
			password=$15,
			disabled=$16,
			next_check_at=$17,
			ignore_http_cache=$18,
			fetch_via_proxy=$19
		WHERE
			id=$20 AND user_id=$21
	`
	_, err = s.db.Exec(query,
		feed.FeedURL,
		feed.SiteURL,
		feed.Title,
		feed.Category.ID,
		feed.EtagHeader,
		feed.LastModifiedHeader,
		feed.CheckedAt,
		feed.ParsingErrorMsg,
		feed.ParsingErrorCount,
		feed.ScraperRules,
		feed.RewriteRules,
		feed.Crawler,
		feed.UserAgent,
		feed.Username,
		feed.Password,
		feed.Disabled,
		feed.NextCheckAt,
		feed.IgnoreHTTPCache,
		feed.FetchViaProxy,
		feed.ID,
		feed.UserID,
	)

	if err != nil {
		return fmt.Errorf(`store: unable to update feed #%d (%s): %v`, feed.ID, feed.FeedURL, err)
	}

	return nil
}

// UpdateFeedError updates feed errors.
func (s *Storage) UpdateFeedError(feed *model.Feed) (err error) {
	query := `
		UPDATE
			feeds
		SET
			parsing_error_msg=$1,
			parsing_error_count=$2,
			checked_at=$3,
			next_check_at=$4
		WHERE
			id=$5 AND user_id=$6
	`
	_, err = s.db.Exec(query,
		feed.ParsingErrorMsg,
		feed.ParsingErrorCount,
		feed.CheckedAt,
		feed.NextCheckAt,
		feed.ID,
		feed.UserID,
	)

	if err != nil {
		return fmt.Errorf(`store: unable to update feed error #%d (%s): %v`, feed.ID, feed.FeedURL, err)
	}

	return nil
}

// RemoveFeed removes a feed.
func (s *Storage) RemoveFeed(userID, feedID int64) error {
	query := `DELETE FROM feeds WHERE id = $1 AND user_id = $2`
	result, err := s.db.Exec(query, feedID, userID)
	if err != nil {
		return fmt.Errorf(`store: unable to remove feed #%d: %v`, feedID, err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(`store: unable to remove feed #%d: %v`, feedID, err)
	}

	if count == 0 {
		return errors.New(`store: no feed has been removed`)
	}

	return nil
}

// ResetFeedErrors removes all feed errors.
func (s *Storage) ResetFeedErrors() error {
	_, err := s.db.Exec(`UPDATE feeds SET parsing_error_count=0, parsing_error_msg=''`)
	return err
}
