package middleware

import (
	"net/http"
	"reflect"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

var validate *validator.Validate

func init() {
	validate = validator.New()
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
}

func ValidationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("validator", validate)
		c.Next()
	}
}

func BindAndValidate(c *gin.Context, obj interface{}) error {
	if err := c.ShouldBind(obj); err != nil {
		return err
	}
	v := c.MustGet("validator").(*validator.Validate)
	if err := v.Struct(obj); err != nil {
		var errors []string
		for _, err := range err.(validator.ValidationErrors) {
			errors = append(errors, formatValidationError(err))
		}
		helpers.SendResponse(c, http.StatusBadRequest, "Validation failed", gin.H{"errors": errors})
		return err
	}
	return nil
}

func formatValidationError(err validator.FieldError) string {
	field := err.Field()
	switch err.Tag() {
	case "required":
		return field + " is required"
	case "email":
		return field + " must be a valid email address"
	case "min":
		return field + " must be at least " + err.Param() + " characters"
	case "max":
		return field + " must be at most " + err.Param() + " characters"
	case "len":
		return field + " must be exactly " + err.Param() + " characters"
	case "numeric":
		return field + " must be numeric"
	case "alpha":
		return field + " must contain only letters"
	case "alphanum":
		return field + " must contain only letters and numbers"
	case "uuid":
		return field + " must be a valid UUID"
	case "url":
		return field + " must be a valid URL"
	case "oneof":
		return field + " must be one of: " + err.Param()
	default:
		return field + " failed validation: " + err.Tag()
	}
}

func SanitizeInput(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func SanitizeMap(m map[string]interface{}) {
	for k, v := range m {
		if str, ok := v.(string); ok {
			m[k] = SanitizeInput(str)
		}
	}
}
