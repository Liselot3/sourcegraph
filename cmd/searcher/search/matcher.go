package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sourcegraph/sourcegraph/pkg/pathmatch"
	"github.com/sourcegraph/sourcegraph/pkg/searcher/protocol"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	otlog "github.com/opentracing/opentracing-go/log"
)

const (
	// maxFileSize is the limit on file size in bytes. Only files smaller
	// than this are searched.
	maxFileSize = 1 << 20 // 1MB; match https://sourcegraph.sgdev.org/search?q=repo:%5Egithub%5C.com/sourcegraph/zoekt%24+%22-file_limit%22

	// maxLineSize is the maximum length of a line in bytes.
	// Lines larger than this are not scanned for results.
	// (e.g. minified javascript files that are all on one line).
	maxLineSize = 500

	// maxFileMatches is the limit on number of matching files we return.
	maxFileMatches = 1000

	// maxLineMatches is the limit on number of matches to return in a
	// file.
	maxLineMatches = 100

	// maxOffsets is the limit on number of matches to return on a line.
	maxOffsets = 10

	// numWorkers is how many concurrent readerGreps run per
	// concurrentFind
	numWorkers = 8
)

// readerGrep is responsible for finding LineMatches. It is not concurrency
// safe (it reuses buffers for performance).
//
// This code is base on reading the techniques detailed in
// http://blog.burntsushi.net/ripgrep/
//
// The stdlib regexp is pretty powerful and in fact implements many of the
// features in ripgrep. Our implementation gives high performance via pruning
// aggressively which files to consider (non-binary under a limit) and
// optimizing for assuming most lines will not contain a match. The pruning of
// files is done by the store.
//
// If there is no more low-hanging fruit and perf is not acceptable, we could
// consider an using ripgrep directly (modify it to search zip archives).
//
// TODO(keegan) return search statistics
type readerGrep struct {
	// re is the regexp to match, or nil if empty ("match all files' content").
	re *regexp.Regexp

	// ignoreCase if true means we need to do case insensitive matching.
	ignoreCase bool

	// transformBuf is reused between file searches to avoid
	// re-allocating. It is only used if we need to transform the input
	// before matching. For example we lower case the input in the case of
	// ignoreCase.
	transformBuf []byte

	// matchPath is compiled from the include/exclude path patterns and reports
	// whether a file path matches (and should be searched).
	matchPath pathmatch.PathMatcher

	// literalSubstring is used to test if a file is worth considering for
	// matches. literalSubstring is guaranteed to appear in any match found by
	// re. It is the output of the longestLiteral function. It is only set if
	// the regex has an empty LiteralPrefix.
	literalSubstring []byte
}

// compile returns a readerGrep for matching p.
func compile(p *protocol.PatternInfo) (*readerGrep, error) {
	var (
		re               *regexp.Regexp
		literalSubstring []byte
	)
	if p.Pattern != "" {
		expr := p.Pattern
		if !p.IsRegExp {
			expr = regexp.QuoteMeta(expr)
		}
		if p.IsWordMatch {
			expr = `\b` + expr + `\b`
		}
		if p.IsRegExp {
			// We don't do the search line by line, therefore we want the
			// regex engine to consider newlines for anchors (^$).
			expr = "(?m:" + expr + ")"
		}
		if !p.IsCaseSensitive {
			// We don't just use (?i) because regexp library doesn't seem
			// to contain good optimizations for case insensitive
			// search. Instead we lowercase the input and pattern.
			re, err := syntax.Parse(expr, syntax.Perl)
			if err != nil {
				return nil, err
			}
			lowerRegexpASCII(re)
			expr = re.String()
		}

		var err error
		re, err = regexp.Compile(expr)
		if err != nil {
			return nil, err
		}

		// Only use literalSubstring optimization if the regex engine doesn't
		// have a prefix to use.
		if pre, _ := re.LiteralPrefix(); pre == "" {
			ast, err := syntax.Parse(expr, syntax.Perl)
			if err != nil {
				return nil, err
			}
			ast = ast.Simplify()
			literalSubstring = []byte(longestLiteral(ast))
		}
	}

	pathOptions := pathmatch.CompileOptions{
		RegExp:        p.PathPatternsAreRegExps,
		CaseSensitive: p.PathPatternsAreCaseSensitive,
	}
	matchPath, err := pathmatch.CompilePathPatterns(p.AllIncludePatterns(), p.ExcludePattern, pathOptions)
	if err != nil {
		return nil, err
	}

	return &readerGrep{
		re:               re,
		ignoreCase:       !p.IsCaseSensitive,
		matchPath:        matchPath,
		literalSubstring: literalSubstring,
	}, nil
}

// Copy returns a copied version of rg that is safe to use from another
// goroutine.
func (rg *readerGrep) Copy() *readerGrep {
	var reCopy *regexp.Regexp
	if rg.re != nil {
		reCopy = rg.re.Copy()
	}
	return &readerGrep{
		re:               reCopy,
		ignoreCase:       rg.ignoreCase,
		matchPath:        rg.matchPath.Copy(),
		literalSubstring: rg.literalSubstring,
	}
}

// matchString returns whether rg's regexp pattern matches s. It is intended to be
// used to match file paths.
func (rg *readerGrep) matchString(s string) bool {
	if rg.re == nil {
		return true
	}
	if rg.ignoreCase {
		s = strings.ToLower(s)
	}
	return rg.re.MatchString(s)
}

// Find returns a LineMatch for each line that matches rg in reader.
// LimitHit is true if some matches may not have been included in the result.
// NOTE: This is not safe to use concurrently.
func (rg *readerGrep) Find(zf *zipFile, f *srcFile) (matches []protocol.LineMatch, limitHit bool, err error) {
	if rg.ignoreCase && rg.transformBuf == nil {
		rg.transformBuf = make([]byte, zf.MaxLen)
	}

	// fileMatchBuf is what we run match on, fileBuf is the original
	// data (for Preview).
	fileBuf := zf.DataFor(f)
	fileMatchBuf := fileBuf

	// If we are ignoring case, we transform the input instead of
	// relying on the regular expression engine which can be
	// slow. compile has already lowercased the pattern. We also
	// trade some correctness for perf by using a non-utf8 aware
	// lowercase function.
	if rg.ignoreCase {
		fileMatchBuf = rg.transformBuf[:len(fileBuf)]
		bytesToLowerASCII(fileMatchBuf, fileBuf)
	}

	// Most files will not have a match and we bound the number of matched
	// files we return. So we can avoid the overhead of parsing out new lines
	// and repeatedly running the regex engine by running a single match over
	// the whole file. This does mean we duplicate work when actually
	// searching for results. We use the same approach when we search
	// per-line. Additionally if we have a non-empty literalSubstring, we use
	// that to prune out files since doing bytes.Index is very fast.
	if bytes.Index(fileMatchBuf, rg.literalSubstring) < 0 {
		return nil, false, nil
	}
	first := rg.re.FindIndex(fileMatchBuf)
	if first == nil {
		return nil, false, nil
	}

	idx := 0
	for i := 0; len(matches) < maxLineMatches; i++ {
		advance, lineBuf, err := bufio.ScanLines(fileBuf, true)
		if err != nil {
			// ScanLines should never return an err
			return nil, false, err
		}
		if advance == 0 { // EOF
			break
		}

		// matchBuf is what we actually match on. We have already done
		// the transform of fileBuf in fileMatchBuf. lineBuf is a
		// prefix of fileBuf, so matchBuf is the corresponding prefix.
		matchBuf := fileMatchBuf[:len(lineBuf)]

		// Advance file bufs in sync
		fileBuf = fileBuf[advance:]
		fileMatchBuf = fileMatchBuf[advance:]

		// Check whether we're before the first match.
		idx += advance
		if idx < first[0] {
			continue
		}

		// Skip lines that are too long.
		if len(matchBuf) > maxLineSize {
			continue
		}

		locs := rg.re.FindAllIndex(matchBuf, maxOffsets)
		if len(locs) > 0 {
			lineLimitHit := len(locs) == maxOffsets
			offsetAndLengths := make([][]int, len(locs))
			for i, match := range locs {
				start, end := match[0], match[1]
				offset := utf8.RuneCount(lineBuf[:start])
				length := utf8.RuneCount(lineBuf[start:end])
				offsetAndLengths[i] = []int{offset, length}
			}
			matches = append(matches, protocol.LineMatch{
				// making a copy of lineBuf is intentional.
				// we are not allowed to use the fileBuf data after the zipFile has been Closed,
				// which currently occurs before Preview has been serialized.
				// TODO: consider moving the call to Close until after we are
				// done with Preview, and stop making a copy here.
				// Special care must be taken to call Close on all possible paths, including error paths.
				Preview:          string(lineBuf),
				LineNumber:       i,
				OffsetAndLengths: offsetAndLengths,
				LimitHit:         lineLimitHit,
			})
		}
	}
	limitHit = len(matches) == maxLineMatches
	return matches, limitHit, nil
}

// FindZip is a convenience function to run Find on f.
func (rg *readerGrep) FindZip(zf *zipFile, f *srcFile) (protocol.FileMatch, error) {
	lm, limitHit, err := rg.Find(zf, f)
	return protocol.FileMatch{
		Path:        f.Name,
		LineMatches: lm,
		LimitHit:    limitHit,
	}, err
}

// concurrentFind searches files in zr looking for matches using rg.
func concurrentFind(ctx context.Context, rg *readerGrep, zf *zipFile, fileMatchLimit int, patternMatchesContent, patternMatchesPaths bool) (fm []protocol.FileMatch, limitHit bool, err error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ConcurrentFind")
	ext.Component.Set(span, "matcher")
	if rg.re != nil {
		span.SetTag("re", rg.re.String())
	}
	span.SetTag("path", rg.matchPath.String())
	defer func() {
		if err != nil {
			ext.Error.Set(span, true)
			span.SetTag("err", err.Error())
		}
		span.Finish()
	}()

	if !patternMatchesContent && !patternMatchesPaths {
		patternMatchesContent = true
	}

	if fileMatchLimit > maxFileMatches || fileMatchLimit <= 0 {
		fileMatchLimit = maxFileMatches
	}

	// If we reach fileMatchLimit we use cancel to stop the search
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		// If a deadline is set, try to finish before the deadline expires.
		timeout := time.Duration(0.9 * float64(deadline.Sub(time.Now())))
		span.LogFields(otlog.Int64("concurrentFindTimeout", int64(timeout)))
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var (
		filesmu   sync.Mutex // protects files
		files     = zf.Files
		matchesmu sync.Mutex // protects matches, limitHit
		matches   = []protocol.FileMatch{}
	)

	if patternMatchesPaths && (!patternMatchesContent || rg.re == nil) {
		// Fast path for only matching file paths (or with a nil pattern, which matches all files,
		// so is effectively matching only on file paths).
		for _, f := range files {
			if rg.matchPath.MatchPath(f.Name) && rg.matchString(f.Name) {
				if len(matches) < fileMatchLimit {
					matches = append(matches, protocol.FileMatch{Path: f.Name})
				} else {
					limitHit = true
					break
				}
			}
		}
		return matches, limitHit, nil
	}

	var (
		done          = ctx.Done()
		wg            sync.WaitGroup
		wgErrOnce     sync.Once
		wgErr         error
		filesSkipped  uint32 // accessed atomically
		filesSearched uint32 // accessed atomically
	)

	// Start workers. They read from files and write to matches.
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(rg *readerGrep) {
			defer wg.Done()

			for {
				// check whether we've been cancelled
				select {
				case <-done:
					return
				default:
				}

				// grab a file to work on
				filesmu.Lock()
				if len(files) == 0 {
					filesmu.Unlock()
					return
				}
				f := &files[0]
				files = files[1:]
				filesmu.Unlock()

				// decide whether to process, record that decision
				if !rg.matchPath.MatchPath(f.Name) {
					atomic.AddUint32(&filesSkipped, 1)
					continue
				}
				atomic.AddUint32(&filesSearched, 1)

				// process
				var fm protocol.FileMatch
				fm, err := rg.FindZip(zf, f)
				if err != nil {
					wgErrOnce.Do(func() {
						wgErr = err
						cancel()
					})
					return
				}
				match := len(fm.LineMatches) > 0
				if !match && patternMatchesPaths {
					// Try matching against the file path.
					match = rg.matchString(f.Name)
					if match {
						fm.Path = f.Name
					}
				}
				if match {
					matchesmu.Lock()
					if len(matches) < fileMatchLimit {
						matches = append(matches, fm)
					} else {
						limitHit = true
						cancel()
					}
					matchesmu.Unlock()
				}
			}
		}(rg.Copy())
	}

	wg.Wait()

	err = wgErr
	if err == nil && ctx.Err() == context.DeadlineExceeded {
		// We stopped early because we were about to hit the deadline.
		err = ctx.Err()
	}

	span.LogFields(
		otlog.Int("filesSkipped", int(atomic.LoadUint32(&filesSkipped))),
		otlog.Int("filesSearched", int(atomic.LoadUint32(&filesSearched))),
	)

	return matches, limitHit, err
}

// lowerRegexpASCII lowers rune literals and expands char classes to include
// lowercase. It does it inplace. We can't just use strings.ToLower since it
// will change the meaning of regex shorthands like \S or \B.
func lowerRegexpASCII(re *syntax.Regexp) {
	for _, c := range re.Sub {
		if c != nil {
			lowerRegexpASCII(c)
		}
	}
	switch re.Op {
	case syntax.OpLiteral:
		// For literal strings we can simplify lower each character.
		for i := range re.Rune {
			re.Rune[i] = unicode.ToLower(re.Rune[i])
		}
	case syntax.OpCharClass:
		l := len(re.Rune)
		for i := 0; i < l; i += 2 {
			// We found a char class that includes a-z. No need to
			// modify.
			if re.Rune[i] <= 'a' && re.Rune[i+1] >= 'z' {
				return
			}
		}
		for i := 0; i < l; i += 2 {
			a, b := re.Rune[i], re.Rune[i+1]
			// This range doesn't include A-Z, so skip
			if a > 'Z' || b < 'A' {
				continue
			}
			simple := true
			if a < 'A' {
				simple = false
				a = 'A'
			}
			if b > 'Z' {
				simple = false
				b = 'Z'
			}
			a, b = unicode.ToLower(a), unicode.ToLower(b)
			if simple {
				// The char range is within A-Z, so we can
				// just modify it to be the equivalent in a-z.
				re.Rune[i], re.Rune[i+1] = a, b
			} else {
				// The char range includes characters outside
				// of A-Z. To be safe we just append a new
				// lowered range which is the intersection
				// with A-Z.
				re.Rune = append(re.Rune, a, b)
			}
		}
	default:
		return
	}
	// Copy to small storage if necessary
	for i := 0; i < 2 && i < len(re.Rune); i++ {
		re.Rune0[i] = re.Rune[i]
	}
}

// longestLiteral finds the longest substring that is guaranteed to appear in
// a match of re.
//
// Note: There may be a longer substring that is guaranteed to appear. For
// example we do not find the longest common substring in alternating
// group. Nor do we handle concatting simple capturing groups.
func longestLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpCapture, syntax.OpPlus:
		return longestLiteral(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return longestLiteral(re.Sub[0])
		}
	case syntax.OpConcat:
		longest := ""
		for _, sub := range re.Sub {
			l := longestLiteral(sub)
			if len(l) > len(longest) {
				longest = l
			}
		}
		return longest
	}
	return ""
}

// readAll will read r until EOF into b. It returns the number of bytes
// read. If we do not reach EOF, an error is returned.
func readAll(r io.Reader, b []byte) (int, error) {
	n := 0
	for {
		if len(b) == 0 {
			// We may be at EOF, but it hasn't returned that
			// yet. Technically r.Read is allowed to return 0,
			// nil, but it is strongly discouraged. If they do, we
			// will just return an err.
			scratch := []byte{'1'}
			_, err := r.Read(scratch)
			if err == io.EOF {
				return n, nil
			}
			return n, errors.New("reader is too large")
		}

		m, err := r.Read(b)
		n += m
		b = b[m:]
		if err != nil {
			if err == io.EOF { // done
				return n, nil
			}
			return n, err
		}
	}
}
