package service

import (
	"context"
	"strings"
	"time"

	"go-common/app/service/main/antispam/model"
	"go-common/app/service/main/antispam/util"

	"go-common/library/log"
)

// UserGeneratedContent .
type UserGeneratedContent interface {
	GetID() int64
	GetOID() int64
	GetSenderID() int64
	GetArea() string
	GetContent() string
}

// Digest .
func (s *SvcImpl) Digest() {
	s.digest(s.UserGeneratedContentChan)
}

func (s *SvcImpl) pushToChan(ugc UserGeneratedContent) {
	select {
	case <-done:
	case s.UserGeneratedContentChan <- ugc:
	default:
		log.Warn("regexp extract chan full, abandon ugc(%v)", ugc)
	}
}

// InWhiteList check if a content match the whitelist regexp
// if match, return the matched regexp's name
func (s *SvcImpl) InWhiteList(k *model.Keyword) (name string, isWhite bool) {
	for _, white := range s.GetRegexpsByAreaAndCondFunc(context.TODO(), k.Area, whiteRegexpsCondFn) {
		log.Info("inside white regexp loop: %s", white)
		if white.FindString(k.Content) != "" {
			log.Info("Keyword match whitelist, content: %s", k.Content)
			return white.Name, true
		}
	}
	return "", false
}

// ExcludeWhitelist .
func (s *SvcImpl) ExcludeWhitelist(in <-chan *model.Keyword) <-chan *model.Keyword {
	out := make(chan *model.Keyword, s.Option.DefaultChanSize)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(out)

		for suspicious := range in {
			if name, isWhite := s.InWhiteList(suspicious); isWhite {
				suspicious.Tag = model.KeywordTagWhite
				suspicious.RegexpName = name
			}
			out <- suspicious
		}
		log.Info("exclude whitelist chan receive cancel signal")
	}()
	return out
}

// Aggregate persist only one keyword captured by regexps chain
// only one keyword generated by same owner will be process
func (s *SvcImpl) Aggregate(in <-chan *model.Keyword) <-chan *model.Keyword {
	out := make(chan *model.Keyword, s.Option.DefaultChanSize)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(out)

		for keyword := range in {
			count, err := s.antiDao.IncrAreaSendersCache(context.TODO(), keyword.Area, keyword.SenderID)
			if err != nil {
				log.Error("%v", err)
				continue
			}
			s.antiDao.AreaSendersExpire(context.TODO(), keyword.Area, keyword.SenderID, 1)
			if count == 1 {
				out <- keyword
			}
			continue
		}
		log.Info("aggregate chan receive cancel signal")
	}()
	return out
}

// ExtractKeyword extract keywords by match content with the limit/restrict regexps
func (s *SvcImpl) ExtractKeyword(in <-chan UserGeneratedContent) <-chan *model.Keyword {
	out := make(chan *model.Keyword, s.Option.DefaultChanSize)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(out)
		for obj := range in {
			s.extractKeyword(obj, out)
		}
		log.Info("Extract keyword chan receive cancel")
	}()
	return out
}

// Ignore block reply which are impossible to contain any keyword
func (s *SvcImpl) Ignore(in <-chan *model.Keyword) <-chan *model.Keyword {
	out := make(chan *model.Keyword, s.Option.DefaultChanSize)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(out)

		for keyword := range in {
			s.ignore(keyword, out)
		}
		log.Info("ignore chan receive cancel signal")
	}()
	return out
}

func (s *SvcImpl) extractKeyword(ugc UserGeneratedContent, out chan<- *model.Keyword) {
	for _, reg := range s.GetRegexpsByAreaAndCondFunc(context.TODO(), ugc.GetArea(), limitRegexpsCondFn) {
		log.Info("inside limit/restrict regexp loop, regexp:%s", reg)

		<-s.tokens
		s.wg.Add(1)
		go func(regex *model.Regexp) {
			defer s.wg.Done()
			defer func() {
				s.tokens <- struct{}{}
			}()

			if hit := regex.FindString(ugc.GetContent()); hit != "" {
				hit = strings.TrimSpace(hit)
				if len(hit) < s.Option.MinKeywordLen {
					return
				}
				k := &model.Keyword{
					Content:       hit,
					SenderID:      ugc.GetSenderID(),
					Area:          ugc.GetArea(),
					OriginContent: ugc.GetContent(),
					CTime:         util.JSONTime(time.Now()),
					RegexpName:    regex.Name,
				}
				switch regex.Operation {
				case model.OperationLimit:
					k.Tag = model.KeywordTagDefaultLimit
				case model.OperationRestrictLimit:
					k.Tag = model.KeywordTagRestrictLimit
				}
				out <- k
			}
		}(reg)
	}
}

func (s *SvcImpl) ignore(keyword *model.Keyword, out chan<- *model.Keyword) {
	if len(keyword.Content) < s.Option.MinKeywordLen {
		log.Warn("content small than %d, ignore", s.Option.MinKeywordLen)
		return
	}
	if util.SameChar(keyword.Content) {
		log.Warn("content consists of repeated chars(%s), will be ignored", keyword.Content)
		return
	}
	rs := s.GetRegexpsByAreaAndCondFunc(context.TODO(), keyword.Area, ignoreRegexpsCondFn)
	// NOTE: this is extremly important
	// otherwise, will block pipeline forever
	if len(rs) == 0 {
		out <- keyword
	}
	for _, reg := range rs {
		log.Info("inside ignore regexp loop: %s", reg)

		<-s.tokens
		s.wg.Add(1)
		go func(regex *model.Regexp) {
			defer s.wg.Done()
			defer func() {
				s.tokens <- struct{}{}
			}()
			if hit := regex.FindString(keyword.Content); hit != "" {
				log.Warn("content %s hit ignore regexp %v", keyword.Content, regex)
				return
			}
			out <- keyword
		}(reg)
	}
}

func (s *SvcImpl) digest(ch <-chan UserGeneratedContent) {
	for k := range s.ExcludeWhitelist(
		s.Ignore(s.Aggregate(s.ExtractKeyword(ch)))) {
		log.Info("Catch keyword %v", k)
		persistedKeyword, err := s.PersistKeyword(context.Background(), k)
		if err != nil {
			log.Error("error Persiskeyword %v, error %v", k, err)
			continue
		}
		if persistedKeyword.Tag != model.KeywordTagWhite && k.SenderID > 0 {
			if err := s.persistSenderIDs(context.TODO(), persistedKeyword.ID, k.SenderID); err != nil {
				log.Error("persistSenderIDs(sender_id: %d) fail, error(%v)", k.SenderID, err)
			}
		}
	}
	log.Info("digest receive cancel signal")
}