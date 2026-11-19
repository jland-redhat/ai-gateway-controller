package dashboard

import (
	"strings"
	"testing"
)

func TestGeoMapUsesRaisedDeepSpaceStyle(t *testing.T) {
	data, err := FS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"var GEO_LAND_COLOR='#394b59';",
		"var GEO_GLOW_STOPS=['#394b59','#8d5b3d','#d47b3e','#ffb36c','#fff2d8'];",
		"background:linear-gradient(90deg,#394b59,#8d5b3d,#d47b3e,#ffb36c,#fff2d8)",
		"d3.interpolateRgbBasis(GEO_GLOW_STOPS)",
		"defs.append('radialGradient').attr('id','geo-glow-grad')",
		"geoHotspotCount(value,max)",
		"geoGlowRadius(value,max)",
		"mix-blend-mode:screen",
		"const countryBorderLayer=svg.append('g').attr('class','geo-country-border-layer');",
		"const stateBorderLayer=svg.append('g').attr('class','geo-state-border-layer');",
		"d3.geoNaturalEarth1().fitExtent",
		"geo-atmosphere",
		"geo-depth-shadow",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("dashboard.html no longer contains %q", want)
		}
	}
	for _, old := range []string{"#39d353", "#9be9a8", "GEO_ZERO_COLOR"} {
		if strings.Contains(string(data), old) {
			t.Errorf("dashboard.html still contains old choropleth color %q", old)
		}
	}
	for _, removed := range []string{"d3.geoGraticule10()", "filter:url(#geo-border-glow)", "const borderGlow="} {
		if strings.Contains(string(data), removed) {
			t.Errorf("dashboard.html still contains removed map treatment %q", removed)
		}
	}
}

func TestGeoMapUsesCountryFillHeat(t *testing.T) {
	data, err := FS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"class','geo-country-heat'",

		"geoSparkLayer=svg.append('g').attr('class','geo-spark-layer')",
		"class','geo-state-heat'",
		"pointer-events:all;transition:stroke .18s,stroke-width .18s,opacity .18s",
		"geo-country.geo-hover",
		"geo-state.geo-hover",
		"geoHoverBorderColor(code)",
		"stroke-width:.24;opacity:1",
		"stroke:#071421;stroke-width:.18;opacity:1",
		"geo-activity-spark",
		"queueGeoFlashes(data)",
		"geoActivityTimer=setTimeout(step,1100)",
		"geo-spark-dot",
		"attr('fill','#ff3b30')",
		"attr('fill','#c81e1e')",
		"var t=geoIntensity(value,max),eased=t*t*(3-2*t),count=geoHotspotCount(value,max);",
		"parts.path.classed('is-on',eased>0)",
		"const countryHeatLayer=svg.append('g').attr('class','geo-country-heat-layer');",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("dashboard.html no longer contains %q", want)
		}
	}
	if !strings.Contains(string(data), "geoDotCluster(") {
		t.Error("dashboard.html no longer renders proportional centroid glow dots")
	}
}

func TestGeoMapUsesCountryTotalWhenUSStateBreakdownIsMissing(t *testing.T) {
	data, err := FS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"geoHasUSStateData=Array.from(byState.keys()).some(function(k){return k.indexOf('US|')===0&&(byState.get(k)||0)>0;});",
		"geoHasUSStateData?(geoValueByState.get('US|'+code)||0):(geoValueByCountry.get('US')||0)",
		"US country total; state breakdown unavailable",
		"geoStateGlows.get(code)",
		"geoHasUSStateData){",
		"const dotCentroid=code==='US'?projection([-74,40.5]):centroid;",
		"attr('viewBox','0 0 1000 500')",
		"slice(0,10)",
		"limit=30",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("dashboard.html no longer contains %q", want)
		}
	}
}
