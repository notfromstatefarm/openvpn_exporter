package exporters

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/mmcloughlin/geohash"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type OpenvpnServerHeader struct {
	LabelColumns []string
	Metrics      []OpenvpnServerHeaderField
}

type OpenvpnServerHeaderField struct {
	Column    string
	Desc      *prometheus.Desc
	ValueType prometheus.ValueType
}

type OpenVPNExporter struct {
	statusPath                 string
	geoIP *GeoIP
	openvpnUpDesc               *prometheus.Desc
	openvpnStatusUpdateTimeDesc *prometheus.Desc
	openvpnConnectedClientsDesc *prometheus.Desc
	openvpnServerHeaders        map[string]OpenvpnServerHeader
}

type GeoIP struct {
	Ip          string  `json:"query"`
	CountryName string  `json:"country"`
	RegionName  string  `json:"regionName"`
	City        string  `json:"city"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Geohash     string
}

var geoCache = map[string]GeoIP{}

func getGeo(address string) (GeoIP, error) {
	geo := GeoIP{}
	if val, ok := geoCache[address]; ok {
		return val, nil
	}

	log.Printf("Resolving %s", address)

	response, err := http.Get("http://ip-api.com/json/" + address)
	if err != nil {
		return geo, err
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return geo, err
	}

	err = json.Unmarshal(body, &geo)
	if err != nil {
		return geo, err
	}

	geo.Geohash = geohash.Encode(geo.Lat, geo.Lon)

	geoCache[address] = geo

	return geo, nil
}

func NewOpenVPNExporter(statusPath string) (*OpenVPNExporter, error) {
	// Metrics exported both for client and server statistics.
	openvpnUpDesc := prometheus.NewDesc(
		prometheus.BuildFQName("openvpn", "", "up"),
		"Whether scraping OpenVPN's metrics was successful.",
		[]string{"server_geohash", "server_city", "server_country", "server_region", "server_public_ip"}, nil)
	openvpnStatusUpdateTimeDesc := prometheus.NewDesc(
		prometheus.BuildFQName("openvpn", "", "status_update_time_seconds"),
		"UNIX timestamp at which the OpenVPN statistics were updated.",
		[]string{"server_geohash", "server_city", "server_country", "server_region", "server_public_ip"}, nil)

	// Metrics specific to OpenVPN servers.
	openvpnConnectedClientsDesc := prometheus.NewDesc(
		prometheus.BuildFQName("openvpn", "", "server_connected_clients"),
		"Number Of Connected Clients",
		[]string{"server_geohash", "server_city", "server_country", "server_region", "server_public_ip"}, nil)

	serverHeaderClientLabels := []string{"server_geohash", "server_city", "server_country", "server_region", "server_public_ip", "common_name", "connection_time", "real_address", "virtual_address", "username", "geohash", "city", "country", "region"}
	serverHeaderClientLabelColumns := []string{"Common Name", "Connected Since (time_t)", "Real Address", "Virtual Address", "Username", "Geohash", "City", "Country", "Region"}
	serverHeaderRoutingLabels := []string{"server_geohash", "server_city", "server_country", "server_region", "server_public_ip", "common_name", "real_address", "virtual_address", "username", "geohash", "city", "country", "region"}
	serverHeaderRoutingLabelColumns := []string{"Common Name", "Real Address", "Virtual Address", "Username", "Geohash", "City", "Country", "Region"}

	openvpnServerHeaders := map[string]OpenvpnServerHeader{
		"CLIENT_LIST": {
			LabelColumns: serverHeaderClientLabelColumns,
			Metrics: []OpenvpnServerHeaderField{
				{
					Column: "Bytes Received",
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName("openvpn", "server", "client_received_bytes_total"),
						"Amount of data received over a connection on the VPN server, in bytes.",
						serverHeaderClientLabels, nil),
					ValueType: prometheus.CounterValue,
				},
				{
					Column: "Bytes Sent",
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName("openvpn", "server", "client_sent_bytes_total"),
						"Amount of data sent over a connection on the VPN server, in bytes.",
						serverHeaderClientLabels, nil),
					ValueType: prometheus.CounterValue,
				},
				{
					Column: "Distance From Server",
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName("openvpn", "server", "client_distance"),
						"Distance from server to client, in meters",
						serverHeaderClientLabels, nil),
					ValueType: prometheus.GaugeValue,
				},
			},
		},
		"ROUTING_TABLE": {
			LabelColumns: serverHeaderRoutingLabelColumns,
			Metrics: []OpenvpnServerHeaderField{
				{
					Column: "Last Ref (time_t)",
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName("openvpn", "server", "route_last_reference_time_seconds"),
						"Time at which a route was last referenced, in seconds.",
						serverHeaderRoutingLabels, nil),
					ValueType: prometheus.GaugeValue,
				},
			},
		},
	}

	geo, err := getGeo("")
	if err != nil {
		log.Printf("Error getting server geo %v", err)
	}
	return &OpenVPNExporter{
		statusPath:                 statusPath,
		geoIP: &geo,
		openvpnUpDesc:               openvpnUpDesc,
		openvpnStatusUpdateTimeDesc: openvpnStatusUpdateTimeDesc,
		openvpnConnectedClientsDesc: openvpnConnectedClientsDesc,
		openvpnServerHeaders:        openvpnServerHeaders,
	}, nil
}

// Converts OpenVPN status information into Prometheus metrics. This
// function automatically detects whether the file contains server or
// client metrics. For server metrics, it also distinguishes between the
// version 2 and 3 file formats.
func (e *OpenVPNExporter) collectStatusFromReader(statusPath string, file io.Reader, ch chan<- prometheus.Metric) error {
	reader := bufio.NewReader(file)
	buf, _ := reader.Peek(18)
	if bytes.HasPrefix(buf, []byte("TITLE,")) {
		// Server statistics, using format version 2.
		return e.collectServerStatusFromReader(reader, ch, ",")
	} else if bytes.HasPrefix(buf, []byte("TITLE\t")) {
		// Server statistics, using format version 3. The only
		// difference compared to version 2 is that it uses tabs
		// instead of spaces.
		return e.collectServerStatusFromReader(reader, ch, "\t")
	} else if bytes.HasPrefix(buf, []byte("OpenVPN STATISTICS")) {
		// Client statistics.
		return fmt.Errorf("client status not supported in this fork")
	} else {
		return fmt.Errorf("unexpected file contents: %q", buf)
	}
}

func hsin(theta float64) float64 {
	return math.Pow(math.Sin(theta/2), 2)
}

func distance(lat1, lon1, lat2, lon2 float64) float64 {
	var la1, lo1, la2, lo2, r float64
	la1 = lat1 * math.Pi / 180
	lo1 = lon1 * math.Pi / 180
	la2 = lat2 * math.Pi / 180
	lo2 = lon2 * math.Pi / 180

	r = 6378100 // Earth radius in METERS

	h := hsin(la2-la1) + math.Cos(la1)*math.Cos(la2)*hsin(lo2-lo1)

	return 2 * r * math.Asin(math.Sqrt(h))
}

// Converts OpenVPN server status information into Prometheus metrics.
func (e *OpenVPNExporter) collectServerStatusFromReader(file io.Reader, ch chan<- prometheus.Metric, separator string) error {
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	headersFound := map[string][]string{}
	// counter of connected client
	numberConnectedClient := 0

	recordedMetrics := map[OpenvpnServerHeaderField][]string{}

	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), separator)
		if fields[0] == "END" && len(fields) == 1 {
			// Stats footer.
		} else if fields[0] == "GLOBAL_STATS" {
			// Global server statistics.
		} else if fields[0] == "HEADER" && len(fields) > 2 {
			// Column names for CLIENT_LIST and ROUTING_TABLE.
			headersFound[fields[1]] = fields[2:]
		} else if fields[0] == "TIME" && len(fields) == 3 {
			// Time at which the statistics were updated.
			timeStartStats, err := strconv.ParseFloat(fields[2], 64)
			if err != nil {
				return err
			}
			ch <- prometheus.MustNewConstMetric(
				e.openvpnStatusUpdateTimeDesc,
				prometheus.GaugeValue,
				timeStartStats,
				e.geoIP.Geohash,
				e.geoIP.City,
				e.geoIP.CountryName,
				e.geoIP.RegionName,
				e.geoIP.Ip)
		} else if fields[0] == "TITLE" && len(fields) == 2 {
			// OpenVPN version number.
		} else if header, ok := e.openvpnServerHeaders[fields[0]]; ok {
			// Entry that depends on a preceding HEADERS directive.
			columnNames, ok := headersFound[fields[0]]
			if !ok {
				return fmt.Errorf("%s should be preceded by HEADERS", fields[0])
			}
			if len(fields) != len(columnNames)+1 {
				return fmt.Errorf("HEADER for %s describes a different number of columns", fields[0])
			}

			// Store entry values in a map indexed by column name.
			columnValues := map[string]string{}
			for _, column := range header.LabelColumns {
				columnValues[column] = ""
			}
			for i, column := range columnNames {
				columnValues[column] = fields[i+1]
			}

			if columnValues["Common Name"] == "UNDEF" || columnValues["Common Name"] == "" {
				continue // skip this 'client'
			}

			if fields[0] == "CLIENT_LIST" {
				numberConnectedClient++
			}

			if columnValues["Real Address"] != "" {
				ip := strings.Split(columnValues["Real Address"], ":")[0]
				geo, err := getGeo(ip)
				if err != nil {
					log.Printf("Error resolving GeoIP: %v", err)
				} else {
					columnValues["Geohash"] = geo.Geohash
					if geo.City != "" {
						columnValues["City"] = geo.City
					} else {
						columnValues["City"] = "Unknown"
					}
					if geo.RegionName != "" {
						columnValues["Region"] = geo.RegionName
					} else {
						columnValues["Region"] = "Unknown"
					}
					if geo.CountryName != "" {
						columnValues["Country"] = geo.CountryName
					} else {
						columnValues["Country"] = "Unknown"
					}
					if e.geoIP.Lon == 0 && e.geoIP.Lat == 0 {
						// don't bother calculating, geoIP didn't resolve
						columnValues["Distance From Server"] = "0"
					} else {
						d := distance(geo.Lat, geo.Lon, e.geoIP.Lat, e.geoIP.Lon)
						columnValues["Distance From Server"] = fmt.Sprintf("%f", d)
					}

				}
			}

			// Extract columns that should act as entry labels.
			labels := []string{e.geoIP.Geohash,
				e.geoIP.City,
				e.geoIP.CountryName,
				e.geoIP.RegionName,
				e.geoIP.Ip}
			for _, column := range header.LabelColumns {
				labels = append(labels, columnValues[column])
			}

			// Export relevant columns as individual metrics.
			for _, metric := range header.Metrics {
				if columnValue, ok := columnValues[metric.Column]; ok {
					if l, _ := recordedMetrics[metric]; ! subslice(labels, l) {
						value, err := strconv.ParseFloat(columnValue, 64)
						if err != nil {
							return err
						}
						ch <- prometheus.MustNewConstMetric(
							metric.Desc,
							metric.ValueType,
							value,
							labels...)
						recordedMetrics[metric] = append(recordedMetrics[metric], labels...)
					} else {
						log.Printf("Metric entry with same labels: %s, %s", metric.Column, labels)
					}
				}
				
			}
		} else {
			return fmt.Errorf("unsupported key: %q", fields[0])
		}
	}
	// add the number of connected client
	ch <- prometheus.MustNewConstMetric(
		e.openvpnConnectedClientsDesc,
		prometheus.GaugeValue,
		float64(numberConnectedClient),
		e.geoIP.Geohash,
		e.geoIP.City,
		e.geoIP.CountryName,
		e.geoIP.RegionName,
		e.geoIP.Ip)
	return scanner.Err()
}

// Does slice contain string
func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// Is a sub-slice of slice
func subslice(sub []string, main []string) bool {
	if len(sub) > len(main) {return false}
	for _, s := range sub {
		if ! contains(main, s) {
			return false
		}
	}
	return true
}

func (e *OpenVPNExporter) collectStatusFromFile(statusPath string, ch chan<- prometheus.Metric) error {
	conn, err := os.Open(statusPath)
	defer conn.Close()
	if err != nil {
		return err
	}
	return e.collectStatusFromReader(statusPath, conn, ch)
}

func (e *OpenVPNExporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.openvpnUpDesc
}

func (e *OpenVPNExporter) Collect(ch chan<- prometheus.Metric) {
	err := e.collectStatusFromFile(e.statusPath, ch)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			e.openvpnUpDesc,
			prometheus.GaugeValue,
			1.0,
			e.geoIP.Geohash,
			e.geoIP.City,
			e.geoIP.CountryName,
			e.geoIP.RegionName,
			e.geoIP.Ip)
	} else {
		log.Printf("Failed to scrape showq socket: %s", err)
		ch <- prometheus.MustNewConstMetric(
			e.openvpnUpDesc,
			prometheus.GaugeValue,
			0.0,
			e.geoIP.Geohash,
			e.geoIP.City,
			e.geoIP.CountryName,
			e.geoIP.RegionName,
			e.geoIP.Ip)
	}
}
