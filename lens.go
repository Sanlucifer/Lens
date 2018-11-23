package lens

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RTradeLtd/Lens/analyzer/images"
	"github.com/RTradeLtd/rtfs"

	"github.com/RTradeLtd/Lens/analyzer/text"
	"github.com/RTradeLtd/Lens/models"
	"github.com/RTradeLtd/Lens/search"
	"github.com/RTradeLtd/Lens/utils"
	"github.com/RTradeLtd/Lens/xtractor/planetary"
	"github.com/RTradeLtd/config"
	"github.com/gofrs/uuid"
	"github.com/ledongthuc/pdf"
)

// Service contains the various components of Lens
type Service struct {
	im rtfs.Manager

	ta *text.Analyzer
	ia *images.Analyzer
	px *planetary.Extractor
	ss *search.Service
}

// ConfigOpts are options used to configure the lens service
type ConfigOpts struct {
	UseChainAlgorithm bool
	DataStorePath     string
	API               APIOpts
}

// APIOpts defines options for the lens API
type APIOpts struct {
	IP   string
	Port string
}

// IndexOperationResponse is the response from a successfuly lens indexing operation
type IndexOperationResponse struct {
	ContentHash string    `json:"lens_object_content_hash"`
	LensID      uuid.UUID `json:"lens_id"`
}

// NewService is used to generate our Lens service
func NewService(opts *ConfigOpts, cfg *config.TemporalConfig) (*Service, error) {
	ta := text.NewTextAnalyzer(opts.UseChainAlgorithm)

	// instantiate ipfs connection
	ipfsAPI := fmt.Sprintf("%s:%s", cfg.IPFS.APIConnection.Host, cfg.IPFS.APIConnection.Port)
	manager, err := rtfs.NewManager(ipfsAPI, nil, 1*time.Minute)
	if err != nil {
		return nil, err
	}
	px, err := planetary.NewPlanetaryExtractor(manager)
	if err != nil {
		return nil, err
	}

	// instantiate service
	ss, err := search.NewService(opts.DataStorePath)
	if err != nil {
		return nil, err
	}
	imagesOpts := &images.ConfigOpts{ModelLocation: "/tmp"}
	ia, err := images.NewAnalyzer(imagesOpts)
	if err != nil {
		return nil, err
	}
	return &Service{
		ta: ta,
		ia: ia,
		px: px,
		ss: ss,
	}, nil
}

// Magnify is used to examine a given content hash, determine if it's parsable
// and returned the summarized meta-data. Returned parameters are in the format of:
// content type, meta-data, error
func (s *Service) Magnify(contentHash string) (string, *models.MetaData, error) {
	if has, err := s.ss.Has(contentHash); err != nil {
		return "", nil, err
	} else if has {
		return "", nil, errors.New("this object has already been indexed")
	}

	contents, err := s.px.ExtractContents(contentHash)
	if err != nil {
		return "", nil, err
	}
	contentType := http.DetectContentType(contents)

	// it will be in the format of `<content-type>; charset=...`
	// we use strings.FieldsFunc to seperate the string, and to be able to exmaine the content type
	parsed := strings.FieldsFunc(contentType, func(r rune) bool { return (r == ';') })
	parsed2 := strings.FieldsFunc(contentType, func(r rune) bool { return (r == '/') })
	var (
		meta     []string
		category string
	)

	switch parsed[0] {
	case "application/pdf":
		category = "pdf"
		reader, err := pdf.NewReader(bytes.NewReader(contents), int64(len(contents)))
		if err != nil {
			return "", nil, err
		}
		b, err := reader.GetPlainText()
		if err != nil {
			return "", nil, err
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(b); err != nil {
			return "", nil, err
		}
		meta = s.ta.Summarize(buf.String(), 0.25)
	default:
		switch parsed2[0] {
		case "text":
			category = "document"
			meta = s.ta.Summarize(string(contents), 0.25)
		case "image":
			category = "image"
			keyword, err := s.ia.ClassifyImage(contents)
			if err != nil {
				return "", nil, err
			}
			meta = []string{keyword}
		default:
			return "", nil, errors.New("unsupported content type for indexing")
		}
	}
	// clear the stored text so we can parse new text later
	s.ta.Clear()
	metadata := &models.MetaData{
		Summary:  utils.Unique(meta),
		MimeType: contentType,
		Category: category,
	}
	return parsed[0], metadata, nil
}

// Store is used to store our collected meta data in a formatted object
func (s *Service) Store(meta *models.MetaData, name string) (*IndexOperationResponse, error) {
	// generate a uuid for the lens object
	id, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}
	// create the lens object
	obj := models.Object{
		LensID:   id,
		Name:     name,
		MetaData: *meta,
	}
	// mrshal the lens object
	marshaled, err := json.Marshal(&obj)
	if err != nil {
		return nil, err
	}
	// iterate over the meta data summary
	for _, v := range meta.Summary {
		// check to see if a keyword with this name already exists
		if has, err := s.ss.Has(v); err != nil {
			return nil, err
		} else if !has {
			// if the keyword does not exist, create the keyword object
			keyObj := models.Keyword{
				Name:            v,
				LensIdentifiers: []uuid.UUID{id},
			}
			keyObjMarshaled, err := json.Marshal(&keyObj)
			if err != nil {
				return nil, err
			}
			if err = s.ss.Put(v, keyObjMarshaled); err != nil {
				return nil, err
			}
			continue
		}

		// keyword exists, get the keyword object from the datastore
		keywordBytes, err := s.ss.Get(v)
		if err != nil {
			return nil, err
		}

		// unmarshal into the keyword object
		var keyword = models.Keyword{}
		if err = json.Unmarshal(keywordBytes, &keyword); err != nil {
			return nil, err
		}

		var detected = false
		for _, v := range keyword.LensIdentifiers {
			// this should never be reached, but it is here for additional checks and balances
			if v == id {
				detected = true
				break
			}
		}
		if detected {
			// this object has already  been indexed for the particular keyword, so we can skip
			continue
		}

		// update the lens identifiers in the keyword object
		keyword.LensIdentifiers = append(keyword.LensIdentifiers, id)
		// TODO: add field to model of content hashes that are mapped in the keyword obj
		keywordMarshaled, err := json.Marshal(keyword)
		if err != nil {
			return nil, err
		}

		// put (aka, update) the keyword object
		if err = s.ss.Put(v, keywordMarshaled); err != nil {
			return nil, err
		}
	}
	// store the name (aka, content hash) of the object so we can avoid duplicate processing in the future
	if err = s.ss.Put(name, []byte(id.String())); err != nil {
		return nil, err
	}
	// store a "mapping" of the lens uuid to its corresponding lens object
	if err = s.ss.Put(id.String(), marshaled); err != nil {
		return nil, err
	}

	// store the lens object in iPFS
	hash, err := s.im.DagPut(marshaled, "json", "cbor")
	if err != nil {
		return nil, err
	}
	return &IndexOperationResponse{
		// this is the hash of the ipld object
		ContentHash: hash,
		LensID:      id,
	}, nil
}

// SearchByKeyName is used to search for an object by key name
func (s *Service) SearchByKeyName(keyname string) ([]byte, error) {
	if has, err := s.ss.Has(keyname); err != nil {
		return nil, err
	} else if !has {
		return nil, errors.New("keyname does not exist")
	}
	return s.ss.Get(keyname)
}

// KeywordSearch is used to search by keyword
func (s *Service) KeywordSearch(keywords []string) ([]models.Object, error) {
	return s.ss.KeywordSearch(keywords)
}
