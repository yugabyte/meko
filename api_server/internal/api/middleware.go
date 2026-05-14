// Package api provides the Echo server, router, and middleware for the
// datapack management API.
package api

import (
	"container/list"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"

	"meko/api_server/internal/auth"
)

// RegisterMiddleware attaches standard middleware to the Echo instance.
//
// The request logger surfaces auth_method and pat_id when they were
// populated by the auth middleware. This makes "all actions taken by
// PAT X" a single grep over the access log instead of a JOIN against
// the audit table -- the audit table stays narrow (lifecycle events
// only), the access log gets the per-request breadcrumb.
func RegisterMiddleware(e *echo.Echo) {
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:   true,
		LogURI:      true,
		LogStatus:   true,
		LogLatency:  true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			attrs := []any{
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				"latency", v.Latency,
				"error", v.Error,
			}
			if method, ok := c.Get(auth.ContextKeyAuthMethod).(string); ok && method != "" {
				attrs = append(attrs, "auth_method", method)
			}
			if patID, ok := c.Get(auth.ContextKeyPATID).(string); ok && patID != "" {
				attrs = append(attrs, "pat_id", patID)
			}
			slog.Info("request", attrs...)
			return nil
		},
	}))
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			echo.GET, echo.POST, echo.PATCH, echo.DELETE, echo.OPTIONS,
		},
		AllowHeaders: []string{
			echo.HeaderContentType, echo.HeaderAuthorization,
		},
	}))
}

// patLimiterCacheCap caps the number of distinct pat_ids the limiter
// tracks at once. A flood of unique tokens (e.g. an attacker rotating
// fake bearers across the prefix space) would otherwise grow the map
// without bound. 10k entries fits comfortably in memory and is
// orders of magnitude above any realistic active-PAT count.
const patLimiterCacheCap = 10_000

// PerPATRateLimiter caps requests per individual PAT.
//
// Behaviour:
//   - Cognito JWT requests pass through (no pat_id on the context).
//   - PAT requests are rate-limited per pat_id at perMin requests per
//     minute with the supplied burst allowance.
//   - Excess requests get 429 + Retry-After: 60.
//
// Implementation is a bounded LRU of *rate.Limiter so a stream of
// unique pat_ids cannot exhaust memory; eviction is FIFO by last-use.
// The lock granularity is the entire LRU because the hot path is
// O(1) and rate.Limiter.Allow() is itself goroutine-safe.
//
// perMin <= 0 disables the limiter (no-op middleware) so operators
// can opt out via flag without code changes.
func PerPATRateLimiter(perMin, burst int) echo.MiddlewareFunc {
	if perMin <= 0 {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return next
		}
	}
	if burst <= 0 {
		burst = 1
	}
	limiters := newPATLimiterCache(patLimiterCacheCap, rate.Limit(float64(perMin)/60.0), burst)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			patID, _ := c.Get(auth.ContextKeyPATID).(string)
			if patID == "" {
				return next(c)
			}
			lim := limiters.get(patID)
			if !lim.Allow() {
				slog.Warn("pat rate limit exceeded",
					"pat_id", patID,
					"path", c.Path(),
					"per_min", perMin,
					"burst", burst,
				)
				c.Response().Header().Set("Retry-After", "60")
				return c.JSON(http.StatusTooManyRequests, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "PAT_RATE_LIMITED",
						"message": "too many requests for this token; retry after " +
							strconv.Itoa(60) + "s",
					},
				})
			}
			return next(c)
		}
	}
}

// patLimiterCache is a tiny LRU<pat_id, *rate.Limiter>. We use the
// stdlib container/list rather than pulling a real LRU dep -- the
// access pattern is simple (get-or-insert with promotion on hit) and
// the size cap is fixed.
type patLimiterCache struct {
	mu       sync.Mutex
	cap      int
	rateLim  rate.Limit
	burst    int
	items    map[string]*list.Element
	eviction *list.List
}

type patLimiterEntry struct {
	patID string
	lim   *rate.Limiter
}

func newPATLimiterCache(cap int, r rate.Limit, b int) *patLimiterCache {
	return &patLimiterCache{
		cap:      cap,
		rateLim:  r,
		burst:    b,
		items:    make(map[string]*list.Element, cap),
		eviction: list.New(),
	}
}

func (c *patLimiterCache) get(patID string) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[patID]; ok {
		c.eviction.MoveToFront(el)
		return el.Value.(*patLimiterEntry).lim
	}
	lim := rate.NewLimiter(c.rateLim, c.burst)
	el := c.eviction.PushFront(&patLimiterEntry{patID: patID, lim: lim})
	c.items[patID] = el
	if c.eviction.Len() > c.cap {
		oldest := c.eviction.Back()
		if oldest != nil {
			c.eviction.Remove(oldest)
			delete(c.items, oldest.Value.(*patLimiterEntry).patID)
		}
	}
	return lim
}

// ManagementRateLimiter returns a rate-limiting middleware sized for
// control-plane / management endpoints (not data-plane access).
//
// Defaults: 10 req/s sustained, burst of 20, 3-minute expiry per client IP.
func ManagementRateLimiter() echo.MiddlewareFunc {
	store := middleware.NewRateLimiterMemoryStoreWithConfig(
		middleware.RateLimiterMemoryStoreConfig{
			Rate:      10,
			Burst:     20,
			ExpiresIn: 3 * time.Minute,
		},
	)
	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Store: store,
		IdentifierExtractor: func(c echo.Context) (string, error) {
			return c.RealIP(), nil
		},
		DenyHandler: func(c echo.Context, identifier string, err error) error {
			slog.Warn("rate limit exceeded",
				"ip", identifier,
				"path", c.Path(),
			)
			return c.JSON(429, map[string]any{
				"status": "error",
				"code":   "RATE_LIMITED",
				"error":  "too many requests — try again shortly",
			})
		},
	})
}
