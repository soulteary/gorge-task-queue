package httpapi

import (
	"net/http"
	"strconv"

	"github.com/soulteary/gorge-task-queue/internal/queue"

	"github.com/labstack/echo/v4"
)

type Deps struct {
	Store queue.Store
	Token string
}

type apiResponse struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func RegisterRoutes(e *echo.Echo, deps *Deps) {
	e.GET("/", healthPing())
	e.GET("/healthz", healthPing())

	g := e.Group("/api/queue")
	g.Use(tokenAuth(deps))

	g.POST("/enqueue", enqueue(deps))
	g.POST("/lease", lease(deps))
	g.POST("/complete", complete(deps))
	g.POST("/fail", fail(deps))
	g.POST("/yield", yield(deps))
	g.POST("/cancel", cancel(deps))
	g.POST("/awaken", awaken(deps))
	g.GET("/stats", stats(deps))
	g.GET("/tasks", listTasks(deps))
	g.GET("/tasks/:id", getTask(deps))
}

func tokenAuth(deps *Deps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if deps.Token == "" {
				return next(c)
			}
			token := c.Request().Header.Get("X-Service-Token")
			if token == "" {
				token = c.QueryParam("token")
			}
			if token == "" || token != deps.Token {
				return c.JSON(http.StatusUnauthorized, &apiResponse{
					Error: &apiError{Code: "ERR_UNAUTHORIZED", Message: "missing or invalid service token"},
				})
			}
			return next(c)
		}
	}
}

func healthPing() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	}
}

func respondOK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, &apiResponse{Data: data})
}

func respondErr(c echo.Context, status int, code, msg string) error {
	return c.JSON(status, &apiResponse{
		Error: &apiError{Code: code, Message: msg},
	})
}

func enqueue(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.EnqueueRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.TaskClass == "" {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskClass is required")
		}

		task, err := deps.Store.Enqueue(c.Request().Context(), &req)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, task)
	}
}

func lease(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.LeaseRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.Limit <= 0 {
			req.Limit = 1
		}

		leaseOwner := c.Request().Header.Get("X-Lease-Owner")
		if leaseOwner == "" {
			leaseOwner = c.RealIP() + ":github.com/soulteary/gorge-task-queue"
		}

		tasks, err := deps.Store.Lease(c.Request().Context(), req.Limit, leaseOwner)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		if tasks == nil {
			tasks = []*queue.Task{}
		}
		return respondOK(c, tasks)
	}
}

func complete(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.CompleteRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.TaskID <= 0 {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskID is required")
		}

		archived, err := deps.Store.Complete(c.Request().Context(), req.TaskID, req.Duration)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, archived)
	}
}

func fail(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.FailRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.TaskID <= 0 {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskID is required")
		}

		err := deps.Store.Fail(c.Request().Context(), &req)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, map[string]string{"status": "ok"})
	}
}

func yield(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.YieldRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.TaskID <= 0 {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskID is required")
		}

		err := deps.Store.Yield(c.Request().Context(), req.TaskID, req.Duration)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, map[string]string{"status": "ok"})
	}
}

func cancel(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.CancelRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if req.TaskID <= 0 {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskID is required")
		}

		archived, err := deps.Store.Cancel(c.Request().Context(), req.TaskID)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, archived)
	}
}

func awaken(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req queue.AwakenRequest
		if err := c.Bind(&req); err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		}
		if len(req.TaskIDs) == 0 {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "taskIDs is required")
		}

		affected, err := deps.Store.Awaken(c.Request().Context(), req.TaskIDs)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, map[string]int64{"awakened": affected})
	}
}

func stats(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		s, err := deps.Store.Stats(c.Request().Context())
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, s)
	}
}

func listTasks(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		offset, _ := strconv.Atoi(c.QueryParam("offset"))
		if limit <= 0 {
			limit = 100
		}

		tasks, err := deps.Store.ListActive(c.Request().Context(), limit, offset)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		if tasks == nil {
			tasks = []*queue.Task{}
		}
		return respondOK(c, tasks)
	}
}

func getTask(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return respondErr(c, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid task ID")
		}

		task, err := deps.Store.GetTask(c.Request().Context(), id)
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		if task == nil {
			return respondErr(c, http.StatusNotFound, "ERR_NOT_FOUND", "task not found")
		}
		return respondOK(c, task)
	}
}
