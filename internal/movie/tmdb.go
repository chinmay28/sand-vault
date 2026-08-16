package movie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Movie Database is what Plex and Jellyfin match films against, and it is
// what this matches against for the same reasons: it is free for personal use,
// it answers with the artwork as well as the text, and the key is the user's
// own rather than one baked into a binary that would be shared by every
// installation and revoked the first time one of them misbehaved.
//
// Two kinds of key work. The v3 key is a bare hex string and travels as a query
// parameter; the v4 read token is a JWT and travels as a bearer header. People
// copy whichever one their eye lands on first in the API settings page, so both
// are accepted and told apart by their shape.

// DefaultBaseURL is the API this talks to. It is a field on the client rather
// than a constant so a test can point it at a local server, which is the only
// way to test this without a key and an internet connection.
const DefaultBaseURL = "https://api.themoviedb.org/3"

// DefaultImageBaseURL is where the artwork is served from. TMDB publishes it
// through a configuration endpoint that has not changed in a decade; asking for
// it on every lookup would be a round-trip to learn a constant.
const DefaultImageBaseURL = "https://image.tmdb.org/t/p"

// PosterSize is the width the poster is fetched at, in pixels.
//
// Bigger than it needs to be, on purpose: what is stored is a 256px thumbnail
// the vault re-encodes itself, and downscaling from 342 gives a cleaner result
// than fetching something close to the target and hoping. Smaller than the
// sizes above it, also on purpose — this is one image per film over somebody's
// home connection, and the difference is invisible at the size it is shown.
const PosterSize = "w342"

// maxPosterBytes caps what will be read from the image host. A w342 poster is
// 30–60 KB; anything approaching this is not one, and the read stops rather
// than letting a third party decide how much memory this costs.
const maxPosterBytes = 4 << 20

// maxCast is how many names the details view carries. A film has hundreds of
// credits and the top of the billing is the part anybody reads.
const maxCast = 8

// ErrNoKey is returned when a lookup is attempted without an API key.
var ErrNoKey = errors.New("no film database key has been set")

// ErrRejectedKey is returned when the database refuses the key it was given.
var ErrRejectedKey = errors.New("the film database rejected this key")

// ErrNotFound is returned when the database has no such film.
var ErrNotFound = errors.New("the film database has no record with that id")

// ErrRateLimited is returned when the database asks to be left alone for a
// while. It is told apart from the rest because a caller sweeping a folder
// should stop on it: every film after this one would meet the same answer,
// only faster.
var ErrRateLimited = errors.New("the film database is rate-limiting this key — try again in a minute")

// Client talks to the film database on behalf of one vault.
type Client struct {
	// Key is the user's own API key or read token.
	Key string

	// HTTP is the client requests go out on. Nil means a client with the
	// timeout below, which is what everything but a test should use.
	HTTP *http.Client

	// BaseURL and ImageBaseURL default to the constants above.
	BaseURL      string
	ImageBaseURL string
}

// RequestTimeout bounds one call to the database. Generous for an API request
// and deliberately so: this runs on somebody's home connection, possibly on a
// Raspberry Pi, possibly behind a VPN.
const RequestTimeout = 20 * time.Second

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: RequestTimeout}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) imageBaseURL() string {
	if c.ImageBaseURL != "" {
		return strings.TrimSuffix(c.ImageBaseURL, "/")
	}
	return DefaultImageBaseURL
}

// bearer reports whether the key is a v4 read token, which goes in a header
// rather than in the query. A JWT is three base64 segments separated by dots
// and a v3 key is 32 hex characters, so the dots settle it.
func (c *Client) bearer() bool { return strings.Count(c.Key, ".") == 2 }

// get performs one API call and decodes the answer.
func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	if strings.TrimSpace(c.Key) == "" {
		return ErrNoKey
	}
	if params == nil {
		params = url.Values{}
	}
	if !c.bearer() {
		params.Set("api_key", c.Key)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.bearer() {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// The URL is in the error net/http builds, and it carries the key when
		// the key is a query parameter. Say where it failed without saying what
		// it was carrying.
		return fmt.Errorf("could not reach the film database: %w", scrub(err, c.Key))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrRejectedKey
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		return fmt.Errorf("the film database answered %s", resp.Status)
	}

	// Bounded: everything here is a small JSON document, and none of it is
	// worth trusting with unbounded memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("reading the film database's answer: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("the film database's answer could not be read: %w", err)
	}
	return nil
}

// scrub keeps a secret out of an error message.
func scrub(err error, secret string) error {
	if secret == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), secret, "…")
	return errors.New(msg)
}

// Ping checks that the key works, without asking about any particular film.
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		Images struct {
			BaseURL string `json:"secure_base_url"`
		} `json:"images"`
	}
	return c.get(ctx, "/configuration", nil, &out)
}

// searchResponse is the shape of a search answer, narrowed to the fields worth
// showing. Everything unlisted is ignored by the decoder.
type searchResponse struct {
	Results []struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		Original    string  `json:"original_title"`
		Overview    string  `json:"overview"`
		ReleaseDate string  `json:"release_date"`
		PosterPath  string  `json:"poster_path"`
		VoteAverage float64 `json:"vote_average"`
		VoteCount   int     `json:"vote_count"`
	} `json:"results"`
}

// Search asks the database what a guess might be. The year narrows it when the
// filename gave one; a search without a year is still worth making, and Best
// sorts out what comes back.
func (c *Client) Search(ctx context.Context, guess Guess) ([]Candidate, error) {
	title := strings.TrimSpace(guess.Title)
	if title == "" {
		return nil, errors.New("nothing to search for")
	}

	params := url.Values{}
	params.Set("query", title)
	params.Set("include_adult", "false")
	if guess.Year > 0 {
		// primary_release_year rather than year: the latter also matches a
		// re-release, which is how a remaster ends up outranking the film.
		params.Set("primary_release_year", strconv.Itoa(guess.Year))
	}

	var resp searchResponse
	if err := c.get(ctx, "/search/movie", params, &resp); err != nil {
		return nil, err
	}

	// A year that matched nothing is a year the filename got wrong — a
	// re-release, a wrongly named rip — rather than a film that does not
	// exist. Ask again without it before giving up.
	if len(resp.Results) == 0 && guess.Year > 0 {
		params.Del("primary_release_year")
		if err := c.get(ctx, "/search/movie", params, &resp); err != nil {
			return nil, err
		}
	}

	out := make([]Candidate, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Candidate{
			TMDBID:     r.ID,
			Title:      r.Title,
			Original:   r.Original,
			Year:       yearOf(r.ReleaseDate),
			Released:   r.ReleaseDate,
			Overview:   r.Overview,
			Rating:     r.VoteAverage,
			Votes:      r.VoteCount,
			PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// detailsResponse is one film, with its credits appended so that the director
// and the cast do not cost a second round-trip.
type detailsResponse struct {
	ID          int     `json:"id"`
	IMDBID      string  `json:"imdb_id"`
	Title       string  `json:"title"`
	Original    string  `json:"original_title"`
	Overview    string  `json:"overview"`
	Tagline     string  `json:"tagline"`
	ReleaseDate string  `json:"release_date"`
	Runtime     int     `json:"runtime"`
	PosterPath  string  `json:"poster_path"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
	Genres      []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Credits struct {
		Cast []struct {
			Name      string `json:"name"`
			Character string `json:"character"`
			Order     int    `json:"order"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	} `json:"credits"`
}

// Details fetches the full record for one film.
func (c *Client) Details(ctx context.Context, id int) (*Info, error) {
	params := url.Values{}
	params.Set("append_to_response", "credits")

	var resp detailsResponse
	if err := c.get(ctx, "/movie/"+strconv.Itoa(id), params, &resp); err != nil {
		return nil, err
	}
	if resp.ID == 0 {
		return nil, ErrNotFound
	}

	info := &Info{
		TMDBID:     resp.ID,
		IMDBID:     resp.IMDBID,
		Title:      resp.Title,
		Original:   resp.Original,
		Year:       yearOf(resp.ReleaseDate),
		Released:   resp.ReleaseDate,
		Tagline:    resp.Tagline,
		Overview:   resp.Overview,
		Runtime:    resp.Runtime,
		Rating:     resp.VoteAverage,
		Votes:      resp.VoteCount,
		PosterPath: resp.PosterPath,
		MatchedAt:  time.Now().UTC(),
	}
	if info.Original == info.Title {
		info.Original = ""
	}
	for _, g := range resp.Genres {
		info.Genres = append(info.Genres, g.Name)
	}
	for _, crew := range resp.Credits.Crew {
		if crew.Job == "Director" {
			info.Directors = append(info.Directors, crew.Name)
		}
	}
	for _, cast := range resp.Credits.Cast {
		if len(info.Cast) >= maxCast {
			break
		}
		info.Cast = append(info.Cast, Credit{Name: cast.Name, Role: cast.Character})
	}
	return info, nil
}

// Poster fetches the artwork at a path the database gave, as image bytes. The
// caller re-encodes them before storing: what comes back is a stranger's file,
// and the vault only ever stores its own.
func (c *Client) Poster(ctx context.Context, posterPath string) ([]byte, error) {
	posterPath = strings.TrimSpace(posterPath)
	if posterPath == "" {
		return nil, errors.New("this film has no poster")
	}
	if !strings.HasPrefix(posterPath, "/") {
		posterPath = "/" + posterPath
	}
	// The path comes from the database's own answer, but it is still somebody
	// else's string arriving over the network: it names a file under the image
	// host and must not be able to name a host of its own.
	if strings.Contains(posterPath, "..") || strings.Contains(posterPath[1:], "/") {
		return nil, fmt.Errorf("the film database gave an artwork path that is not one: %q", posterPath)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.imageBaseURL()+"/"+PosterSize+posterPath, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch the poster: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the artwork host answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPosterBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the poster: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("the artwork host sent an empty poster")
	}
	return data, nil
}

// Lookup is the whole matching path for one file: guess, search, choose,
// fetch. It is what a sweep runs per film and what correcting a match runs
// with the corrected query.
//
// A search that finds nothing is not an error — most folders hold something
// that is not a film — so it answers with a nil Info and no error, and the
// caller reports it as unmatched.
func (c *Client) Lookup(ctx context.Context, guess Guess) (*Info, error) {
	candidates, err := c.Search(ctx, guess)
	if err != nil {
		return nil, err
	}
	best := Best(candidates, guess)
	if best == nil {
		return nil, nil
	}

	info, err := c.Details(ctx, best.TMDBID)
	if err != nil {
		return nil, err
	}
	info.Query = guess.String()
	return info, nil
}

// yearOf reads the year off a "2006-05-24" release date, which is how every
// date in this API arrives. An unreleased film has no date at all.
func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return year
}
