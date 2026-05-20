package api

import "github.com/gofiber/fiber/v3"

type ErrorBody struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func data(c fiber.Ctx, value any) error {
	return c.JSON(fiber.Map{"data": value})
}

func created(c fiber.Ctx, value any) error {
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": value})
}

func list(c fiber.Ctx, value any, limit int, next string) error {
	return c.JSON(fiber.Map{
		"data": value,
		"pagination": fiber.Map{
			"next":  next,
			"limit": limit,
		},
	})
}

func empty(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"data": fiber.Map{}})
}

func problem(c fiber.Ctx, status int, code string, message string) error {
	return c.Status(status).JSON(ErrorBody{Error: APIError{Code: code, Message: message, Details: map[string]any{}}})
}
