package templates

import (
	"bytes"
	"errors"
	"github.com/kabukky/journey/database"
	"github.com/kabukky/journey/filenames"
	"github.com/kabukky/journey/flags"
	"github.com/kabukky/journey/helpers"
	"github.com/kabukky/journey/structure"
	"gopkg.in/fsnotify.v1"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// For watching the theme directory for changes
var themeFileWatcher *fsnotify.Watcher
var watchedDirectories []string

// For parsing of the theme files
var openTag = []byte("{{")
var closeTag = []byte("}}")
var twoPartArgumentChecker = regexp.MustCompile("(\\S+?)\\s*?=\\s*?['\"](.*?)['\"]")
var quoteTagChecker = regexp.MustCompile("(.*?)[\"'](.+?)[\"']$")

func getFunction(name string) func(*structure.Helper, *structure.RequestData) []byte {
	if helperFuctions[name] != nil {
		return helperFuctions[name]
	} else {
		return helperFuctions["null"]
	}
}

func createHelper(helperName []byte, unescaped bool, startPos int, block []byte, children []structure.Helper, elseHelper *structure.Helper) *structure.Helper {
	var helper *structure.Helper
	// Check for =arguments
	twoPartArgumentResult := twoPartArgumentChecker.FindAllSubmatch(helperName, -1)
	twoPartArguments := make([][]byte, 0)
	for _, arg := range twoPartArgumentResult {
		if len(arg) == 3 {
			twoPartArguments = append(twoPartArguments, bytes.Join(arg[1:], []byte("=")))
			//remove =argument from helper name
			helperName = bytes.Replace(helperName, arg[0], []byte(""), 1)
		}
	}
	// Separate arguments (e.g. 'if @blog.title')
	tags := bytes.Fields(helperName)
	for index, tag := range tags {
		//remove "" around tag if present
		quoteTagResult := quoteTagChecker.FindSubmatch(tag)
		if len(quoteTagResult) != 0 {
			tag = quoteTagResult[1]
		}
		//TODO: This may have to change if the first argument is surrounded by quotes
		if index == 0 {
			helper = makeHelper(string(tag), unescaped, startPos, block, children)
		} else {
			// Handle whitespaces in arguments
			helper.Arguments = append(helper.Arguments, *makeHelper(string(tag), unescaped, 0, []byte{}, nil))
		}
	}
	if len(twoPartArguments) != 0 {
		for _, arg := range twoPartArguments {
			// Check for quotes in the =argument (has beem omitted from the check above)
			quoteTagResult := quoteTagChecker.FindSubmatch(arg)
			if len(quoteTagResult) != 0 {
				// Join poth parts, this time without the youtes
				arg = bytes.Join([][]byte{quoteTagResult[1], quoteTagResult[2]}, []byte(""))
			}
			helper.Arguments = append(helper.Arguments, *makeHelper(string(arg), unescaped, 0, []byte{}, nil))
		}
	}
	if elseHelper != nil {
		helper.Arguments = append(helper.Arguments, *elseHelper)
	}
	return helper
}

func makeHelper(tag string, unescaped bool, startPos int, block []byte, children []structure.Helper) *structure.Helper {
	return &structure.Helper{Name: tag, Arguments: nil, Unescaped: unescaped, Position: startPos, Block: block, Children: children, Function: getFunction(tag)}
}

func findHelper(data []byte, allHelpers []structure.Helper) ([]byte, []structure.Helper) {
	startPos := bytes.Index(data, openTag)
	endPos := bytes.Index(data, closeTag)
	if startPos != -1 && endPos != -1 {
		openTagLength := len(openTag)
		closeTagLength := len(closeTag)
		unescaped := false
		helperName := data[startPos+openTagLength : endPos]
		// Check if helper calls for unescaped text (e.g. three brackets - {{{title}}})
		if bytes.HasPrefix(helperName, []byte("{")) {
			unescaped = true
			openTagLength++ //not necessary
			closeTagLength++
			helperName = helperName[len([]byte("{")):]
		}
		helperName = bytes.Trim(helperName, " ") //make sure there are no trailing whitespaces
		// Remove helper from data
		parts := [][]byte{data[:startPos], data[endPos+closeTagLength:]}
		data = bytes.Join(parts, []byte(""))
		// Check if comment
		if bytes.HasPrefix(helperName, []byte("! ")) || bytes.HasPrefix(helperName, []byte("!--")) {
			return findHelper(data, allHelpers)
		}
		// Check if block
		if bytes.HasPrefix(helperName, []byte("#")) {
			helperName = helperName[len([]byte("#")):] //remove '#' from helperName
			var helper structure.Helper
			data, helper = findBlock(data, helperName, unescaped, startPos) //only use the data string after the opening tag
			allHelpers = append(allHelpers, helper)
			return findHelper(data, allHelpers)
		}
		allHelpers = append(allHelpers, *createHelper(helperName, unescaped, startPos, []byte{}, nil, nil))
		return findHelper(data, allHelpers)
	} else {
		return data, allHelpers
	}
}

func findBlock(data []byte, helperName []byte, unescaped bool, startPos int) ([]byte, structure.Helper) {
	arguments := bytes.Fields(helperName)
	tag := arguments[0] // Get only the first tag (e.g. 'if' in 'if @blog.cover')
	arguments = arguments[1:]
	closeParts := []string{"{{2,3}\\s*/", string(tag), ".?}{2,3}"}
	openParts := []string{"{{2,3}\\s*#", string(tag), ".+?}{2,3}"}
	closeRegex := regexp.MustCompile(strings.Join(closeParts, ""))
	openRegex := regexp.MustCompile(strings.Join(openParts, ""))
	closePositions := closeRegex.FindAllIndex(data, -1)
	openPositions := openRegex.FindAllIndex(data, -1)
	// Check if there are opening tags before the closing tag
	positionIndex := 0
	for _, openPosition := range openPositions {
		if openPosition[0] < closePositions[positionIndex][0] {
			positionIndex++
		}
	}
	block := data[startPos:closePositions[positionIndex][0]]
	parts := [][]byte{data[:startPos], data[closePositions[positionIndex][1]:]}
	data = bytes.Join(parts, []byte(""))
	children := make([]structure.Helper, 0)
	block, children = findHelper(block, children)
	// Handle else (search children for else helper)
	for index, child := range children {
		if child.Name == "else" {
			elseHelper := child
			// Change blocks
			elseHelper.Block = block[elseHelper.Position:]
			block = block[:elseHelper.Position]
			// Change children, omit else helper
			elseHelper.Children = children[(index + 1):]
			// Change Position in children of else helper
			for indexElse, _ := range elseHelper.Children {
				elseHelper.Children[indexElse].Position = elseHelper.Children[indexElse].Position - elseHelper.Position
			}
			children = children[:index]
			helper := createHelper(helperName, unescaped, startPos, block, children, &elseHelper)
			return data, *helper
		}
	}
	helper := createHelper(helperName, unescaped, startPos, block, children, nil)
	return data, *helper
}

func compileTemplate(data []byte, name string) *structure.Helper {
	baseHelper := structure.Helper{Name: name, Arguments: nil, Unescaped: false, Position: 0, Block: []byte{}, Children: nil, Function: getFunction(name)}
	allHelpers := make([]structure.Helper, 0)
	data, allHelpers = findHelper(data, allHelpers)
	baseHelper.Block = data
	baseHelper.Children = allHelpers
	// Handle extend helper
	for index, child := range baseHelper.Children {
		if child.Name == "body" {
			baseHelper.BodyHelper = &baseHelper.Children[index]
		}
	}
	return &baseHelper
}

func createTemplateFromFile(filename string) (*structure.Helper, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	fileNameWithoutExtension := helpers.GetFilenameWithoutExtension(filename)
	// Check if a helper with the same name is already in the map
	if compiledTemplates.m[fileNameWithoutExtension] != nil {
		return nil, errors.New("Error: Conflicting .hbs name '" + fileNameWithoutExtension + "'. A theme file of the same name already exists.")
	}
	helper := compileTemplate(data, fileNameWithoutExtension)
	return helper, nil
}

func inspectTemplateFile(filePath string, info os.FileInfo, err error) error {
	if !info.IsDir() && filepath.Ext(filePath) == ".hbs" {
		helper, err := createTemplateFromFile(filePath)
		if err != nil {
			return err
		}
		compiledTemplates.m[helper.Name] = helper
	}
	return nil
}

func Generate() error {
	compiledTemplates.Lock()
	defer compiledTemplates.Unlock()
	activeTheme, err := database.RetrieveActiveTheme()
	if err != nil {
		return err
	}
	// Compile all template files
	// First clear compiledTemplates map (theme could have been changed)
	compiledTemplates.m = make(map[string]*structure.Helper)
	currentThemePath := filepath.Join(filenames.ThemesFilepath, *activeTheme)
	// Check if the theme folder exists
	if _, err := os.Stat(currentThemePath); os.IsNotExist(err) {
		log.Fatal("Error: Couldn't find theme files in " + currentThemePath + ": " + err.Error())
		return err
	}
	err = filepath.Walk(currentThemePath, inspectTemplateFile)
	if err != nil {
		return err
	}
	// Check if index and post templates are compiled
	if _, ok := compiledTemplates.m["index"]; !ok {
		return errors.New("Couldn't compile template 'index'. Is index.hbs missing?")
	}
	if _, ok := compiledTemplates.m["post"]; !ok {
		return errors.New("Couldn't compile template 'post'. Is post.hbs missing?")
	}
	// If the dev flag is set, watch the theme directory for changes
	if flags.IsInDevMode {
		err = watchThemeDirectory(currentThemePath)
		if err != nil {
			return err
		}
	}
	return nil
}

func watchThemeDirectory(currentThemePath string) error {
	// Prepare watcher to generate the theme on changes to the files
	if themeFileWatcher == nil {
		var err error
		themeFileWatcher, err = createThemeFileWatcher()
		if err != nil {
			return err
		}
	} else {
		// Remove all current directories from watcher
		for _, dir := range watchedDirectories {
			err := themeFileWatcher.Remove(dir)
			if err != nil {
				return err
			}
		}
	}
	watchedDirectories = make([]string, 0)
	// Watch all subdirectories in theme directory
	err := filepath.Walk(currentThemePath, func(filePath string, info os.FileInfo, err error) error {
		if info.IsDir() {
			err := themeFileWatcher.Add(filePath)
			if err != nil {
				return err
			}
			watchedDirectories = append(watchedDirectories, filePath)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func createThemeFileWatcher() (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Write == fsnotify.Write && filepath.Ext(event.Name) == ".hbs" {
					go Generate()
				}
			case err := <-watcher.Errors:
				log.Println("Error while watching theme directory.", err)
			}
		}
	}()
	return watcher, nil
}
