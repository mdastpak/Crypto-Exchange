package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The personal favorites of a user (#217).

// FavoritesOrder is the new order of the favorites of each kind. A kind that
// is left out keeps its order; a kind that is sent names each of its
// favorites once.
type FavoritesOrder struct {
	Currencies []string `json:"currencies,omitempty"`
	Pairs      []string `json:"pairs,omitempty"`
}

// FavoriteChange answers an add or a remove.
type FavoriteChange struct {
	// Changed is false when the favorite already was, or was not, there.
	Changed bool `json:"changed"`
}

func maxFavorites(cfg *config.Configuration) int {
	if n := cfg.Favorites.MaxPerUser; n > 0 {
		return n
	}
	return config.DefaultMaxFavoritesPerUser
}

// ListFavorites returns the favorites of the signed-in user.
//
// @Summary List my favorites
// @Description The favorite currencies and trading pairs of the signed-in user, each kind in the user's order, with the state of each now: a favorite of a currency that was later suspended or deleted stays, with that status.
// @Tags user favorites
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=favorites.List}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites [get]
func (h *UserHandler) ListFavorites(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := favorites.Get(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to list the favorites of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_FAVORITES"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("FAVORITES_RETRIEVED_SUCCESSFULLY"), list)
}

// targetParam reads the currency or pair of the path, in its canonical form.
func targetParam(c *gin.Context) (string, bool) {
	id, err := uuid.Parse(c.Param("hk"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_UUID_FORMAT"), nil)
		return "", false
	}
	return id.String(), true
}

func (h *UserHandler) changeFavorite(c *gin.Context, kind favorites.Kind, add bool) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	target, ok := targetParam(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var changed bool
	var err error
	if add {
		changed, err = favorites.Add(ctx, h.PostgresDB.DB, userID.String(), kind, target, maxFavorites(h.settings()))
	} else {
		changed, err = favorites.Remove(ctx, h.PostgresDB.DB, userID.String(), kind, target)
	}
	switch {
	case errors.Is(err, favorites.ErrNotListed):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M(i18n.Key("FAVORITE_NOT_LISTED_"+strings.ToUpper(string(kind)))), nil)
	case errors.Is(err, favorites.ErrLimit):
		helpers.SendError(c, http.StatusConflict, "favorites_limit_reached", i18n.M("FAVORITES_LIMIT_REACHED"),
			gin.H{"max_favorites": maxFavorites(h.settings())})
	case errors.Is(err, favorites.ErrUserNotFound):
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_NOT_AVAILABLE_CONTACT_SUPPORT"), nil)
	case err != nil:
		log.Printf("Failed to change a favorite: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CHANGE_THE_FAVORITE"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("FAVORITES_UPDATED"), FavoriteChange{Changed: changed})
	}
}

// @Summary Add a favorite currency
// @Description Adds an approved currency at the end of the favorite currencies. A repeated add answers 200 with changed false. The favorites of a user, currencies and pairs together, are capped by favorites.max_per_user; an add past it answers 409 favorites_limit_reached, also under concurrent adds on several instances.
// @Tags user favorites
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's hk" format(uuid)
// @Success 200 {object} models.APIResponse{info=FavoriteChange}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "Not an approved currency"
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites/currencies/{hk} [put]
func (h *UserHandler) AddFavoriteCurrency(c *gin.Context) {
	h.changeFavorite(c, favorites.KindCurrency, true)
}

// @Summary Remove a favorite currency
// @Description A repeated remove answers 200 with changed false.
// @Tags user favorites
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's hk" format(uuid)
// @Success 200 {object} models.APIResponse{info=FavoriteChange}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites/currencies/{hk} [delete]
func (h *UserHandler) RemoveFavoriteCurrency(c *gin.Context) {
	h.changeFavorite(c, favorites.KindCurrency, false)
}

// @Summary Add a favorite trading pair
// @Description Adds an active pair at the end of the favorite pairs, as for currencies.
// @Tags user favorites
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The pair's id" format(uuid)
// @Success 200 {object} models.APIResponse{info=FavoriteChange}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "Not an active pair"
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites/pairs/{hk} [put]
func (h *UserHandler) AddFavoritePair(c *gin.Context) {
	h.changeFavorite(c, favorites.KindPair, true)
}

// @Summary Remove a favorite trading pair
// @Tags user favorites
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The pair's id" format(uuid)
// @Success 200 {object} models.APIResponse{info=FavoriteChange}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites/pairs/{hk} [delete]
func (h *UserHandler) RemoveFavoritePair(c *gin.Context) {
	h.changeFavorite(c, favorites.KindPair, false)
}

// @Summary Reorder my favorites
// @Description Sets the order of the favorite currencies, the favorite pairs, or both. A kind that is sent names each of its favorites exactly once, or the request answers 409 order_mismatch and changes nothing, so that a reorder based on a stale list does not drop or keep a favorite by mistake.
// @Tags user favorites
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body FavoritesOrder true "The new order"
// @Success 200 {object} models.APIResponse{info=favorites.List}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/favorites/order [put]
func (h *UserHandler) ReorderFavorites(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	var req FavoritesOrder
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	canonical := func(field string, ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, raw := range ids {
			id, err := uuid.Parse(raw)
			if err != nil {
				errs[field] = "must be UUIDs"
				return nil
			}
			out = append(out, id.String())
		}
		return out
	}
	currencies, pairs := canonical("currencies", req.Currencies), canonical("pairs", req.Pairs)
	if req.Currencies == nil && req.Pairs == nil {
		errs["currencies"] = "or pairs is required"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	for _, change := range []struct {
		kind favorites.Kind
		ids  []string
		sent bool
	}{{favorites.KindCurrency, currencies, req.Currencies != nil}, {favorites.KindPair, pairs, req.Pairs != nil}} {
		if !change.sent {
			continue
		}
		err := favorites.Reorder(ctx, h.PostgresDB.DB, userID.String(), change.kind, change.ids)
		switch {
		case errors.Is(err, favorites.ErrOrderMismatch):
			helpers.SendError(c, http.StatusConflict, "order_mismatch",
				i18n.M("FAVORITES_ORDER_MISMATCH", i18n.A("kind", i18n.Key("FAVORITE_KIND_"+strings.ToUpper(string(change.kind))))), nil)
			return
		case errors.Is(err, favorites.ErrUserNotFound):
			helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_NOT_AVAILABLE_CONTACT_SUPPORT"), nil)
			return
		case err != nil:
			log.Printf("Failed to reorder the favorites of a user: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_REORDER_FAVORITES"), nil)
			return
		}
	}
	list, err := favorites.Get(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to list the favorites of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_FAVORITES"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("FAVORITES_REORDERED"), list)
}
