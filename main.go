package main

import (
	"fmt"
	"hash/crc32"
	"strconv"
	"bytes"
	"encoding/binary"
	"log"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	Version    = "0.0.29"
	SHM_DEYE   = 0x1238
	SHM_METER  = 0x1264
	SHM_ETEK   = 0x1230
	SHM_CMD    = 0x1231
	SIZE_DEYE  = 512
	SIZE_METER = 64
	SIZE_ETEK  = 1024
	SIZE_CMD   = 64
)

// ShmPayload sincronizada con deye-ctl v0.0.9
type ShmPayload struct {
	Timestamp   int64
	GenInv      float64
	Grid        float64
	BattPower   float64
	SOC         float64
	GridCTInt   float64 // Registro 169
	LoadTotal   float64
	TempDisipDC float64
	TempBatt    float64
	PV1Power    float64
	PV2Power    float64
	PV3Power    float64
	PV4Power    float64
	TempDisipAC float64
	InvOutPower float64 // Registro 175
	Padding     [388]byte
	CRC         uint32
}

type MeterPayload struct {
	Timestamp int64; ActivePower, ReactivePower, Voltage, Current, Frequency, PowerFactor, ImportActiveEnergy, ExportActiveEnergy, ImportReactiveEnergy, ExportReactiveEnergy, PotenciaMediaImportada, PotenciaMediaExportada float32
	Ventana int32; Crc32 uint32
}

type EVSEStatus struct {
	ModbusID         uint8
	Reserved         uint8
	WorkingStatus    uint16
	MaxChargeCurrent uint16
	OutputPWMDuty    uint16
	RotarySwitchPWM  uint16
	RemoteStartStop  uint16
	Pad              [20]byte
}

type EtekPayload struct {
	Timestamp       int64
	ActivePower     float32
	IntegratedPower float32
	LimitW          float32
	MarginW         float32
	NumControllers  int32
	CommonPad       [32]byte
	Controllers     [4]EVSEStatus
	Crc32           uint32
}

type CommandPayload struct {
	Action      int32
	Value1      int32
	Value2      int32
	TimestampTx int64
	Source      [24]byte
	TimestampRx int64
	Pad         [8]byte
	Crc32       uint32
}

var (
	addrDeye uintptr
	addrMeter uintptr
	addrEtek uintptr
	addrCmd  uintptr
)

// attachSHM intenta obtener y mapear un segmento de memoria compartida
func attachSHM(key uint32, size int) uintptr {
	r1, _, _ := syscall.Syscall(syscall.SYS_SHMGET, uintptr(key), uintptr(size), 0)
	shmid := int(r1)
	if shmid < 0 { return 0 }
	addr, _, _ := syscall.Syscall(syscall.SYS_SHMAT, uintptr(shmid), 0, 0)
	if addr == ^uintptr(0) { return 0 }
	return addr
}

const htmlRaw = `<!DOCTYPE html>
<html>
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
	<title>Deye Control</title>
	<meta http-equiv="refresh" content="1">
	<style>
		body { font-family: monospace; background: #010409; color: #c9d1d9; margin: 0; padding: 8px; font-size: 13px; }
		.top-bar { color: #8b949e; font-size: 0.9em; margin-bottom: 10px; text-align: center; border-bottom: 1px solid #30363d; padding-bottom: 4px; }
		.card { background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 10px; margin-bottom: 8px; max-width: 450px; margin: auto; }
		.card-header { display: flex; justify-content: space-between; align-items: baseline; margin-bottom: 8px; border-bottom: 1px solid #21262d; padding-bottom: 3px; }
		h2 { color: #58a6ff; font-size: 0.9em; margin: 0; text-transform: uppercase; }
		.ts { font-size: 0.85em; color: #8b949e; }
		.err { color: #f85149; font-weight: bold; }
		.row { display: flex; justify-content: space-between; padding: 4px 0; }
		.lab { color: #8b949e; }
		.val { font-weight: bold; color: #fff; }
		.btn-group { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 4px; margin-top: 8px; }
		button { background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 12px 2px; border-radius: 4px; font-size: 0.85em; cursor: pointer; font-family: monospace; }
		.btn-stop { color: #f85149; }
	</style>
</head>
<body>
	<div class="top-bar">Deye Eastron ETEK Control v{VER}</div>

	<div class="card">
		<div class="card-header">
			<h2>Medidor de Red (SDM)</h2>
			<span class="ts">{M_TS}</span>
		</div>
		<div class="row"><span class="lab">Activa Instantánea</span><span class="val" style="color:#ffa657; font-size:1.2em;">{M_ACT} W</span></div>
		<div class="row"><span class="lab">Medias (Imp|Exp|Vent)</span><span class="val">{M_MIMP} | {M_MEXP} | {M_WIN}s</span></div>
		<div class="row"><span class="lab">Datos Red (V|A)</span><span class="val">{M_VOLT} V | {M_AMP} A</span></div>
	</div>

	<div class="card">
		<div class="card-header">
			<h2>Inversor Deye</h2>
			<span class="ts {D_ERR_CLS}">{D_TS_MSG}</span>
		</div>
		<div class="row"><span class="lab">Solar PV | Gen-Micro</span><span class="val">{D_PV} W | {D_GEN} W</span></div>
		<div class="row"><span class="lab">Grid (R172) | CT-int (R169)</span><span class="val">{D_GRID} W | {D_GCT} W</span></div>
		<div class="row"><span class="lab">Inv-Output (R175)</span><span class="val">{D_IOUT} W</span></div>
		<div class="row">
			<span class="lab">Batería | SOC</span>
			<span class="val"><span style="color:{D_BAT_COL}">{D_BAT} W</span> | {D_SOC}%</span>
		</div>
		<div class="row"><span class="lab">Carga (Total)</span><span class="val">{D_LOAD} W</span></div>
		<div class="row"><span class="lab">Temps (DC|AC|Bat)</span><span class="val">{D_TDC}º | {D_TAC}º | {D_TBAT}º</span></div>
	</div>

	<div class="card">
		<div class="card-header">
			<h2>Control EVSE</h2>
			<span class="ts">{E_TS}</span>
		</div>
		<div class="row"><span class="lab">Algoritmo | Margen</span><span class="val">{E_STAT} | {E_MARG} W</span></div>
		<div class="row" style="margin-bottom:6px;"><span class="lab">Límite | Media 12s</span><span class="val">{E_LIM} W | {E_PINT} W</span></div>
		{E_CTRLS}
	</div>
</body>
</html>`

func main() {
	http.HandleFunc("/", handleUI)
	
	// Manejador de comandos EVSE (v0.0.28)
	http.HandleFunc("/evse/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 { return }
		
		actionStr := parts[1] // "start" o "stop"
		idStr := r.URL.Query().Get("id")
		modbusID, _ := strconv.Atoi(idStr)

		if modbusID == 0 { return }

		var action int32
		if actionStr == "start" {
			action = 1
		} else if actionStr == "stop" {
			action = 2
		} else {
			return
		}

		cmd := CommandPayload{
			Action:      action,
			Value1:      int32(modbusID),
			Value2:      0,
			TimestampTx: time.Now().Unix(),
		}
		copy(cmd.Source[:], []byte("dde-iu"))

		// Escritura en SHM 0x1231
		if addrCmd == 0 { addrCmd = attachSHM(SHM_CMD, SIZE_CMD) }
		if addrCmd != 0 {
			cmd.Crc32 = 0
			var buf bytes.Buffer
			binary.Write(&buf, binary.LittleEndian, cmd)
			b := buf.Bytes()
			cmd.Crc32 = crc32.ChecksumIEEE(b[:60])
			
			buf.Reset()
			binary.Write(&buf, binary.LittleEndian, cmd)
			
			dst := unsafe.Slice((*byte)(unsafe.Pointer(addrCmd)), SIZE_CMD)
			copy(dst, buf.Bytes())
			fmt.Printf("Comando EVSE %s (ID %d) enviado a SHM 0x1231\n", actionStr, modbusID)
		}
		fmt.Fprint(w, "OK")
	})

	fmt.Printf("Deye UI v%s | Full Display | Port 80\n", Version)
	log.Fatal(http.ListenAndServe(":80", nil))
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	d := readDeyeSHM()
	m := readMeterSHM()
	e := readEtekSHM()

	res := htmlRaw
	res = strings.ReplaceAll(res, "{VER}", Version)
	
	// --- SECCIÓN METER ---
	res = strings.ReplaceAll(res, "{M_TS}", time.Unix(m.Timestamp, 0).Format("15:04:05"))
	res = strings.ReplaceAll(res, "{M_ACT}", fmt.Sprintf("%.1f", m.ActivePower))
	res = strings.ReplaceAll(res, "{M_MIMP}", fmt.Sprintf("%.1f", m.PotenciaMediaImportada))
	res = strings.ReplaceAll(res, "{M_MEXP}", fmt.Sprintf("%.1f", m.PotenciaMediaExportada))
	res = strings.ReplaceAll(res, "{M_WIN}", fmt.Sprintf("%d", m.Ventana))
	res = strings.ReplaceAll(res, "{M_VOLT}", fmt.Sprintf("%.1f", m.Voltage))
	res = strings.ReplaceAll(res, "{M_AMP}", fmt.Sprintf("%.2f", m.Current))

	// --- SECCIÓN INVERSOR ---
	tsMsg := time.Unix(d.Timestamp, 0).Format("15:04:05")
	errCls := ""
	if !validateDeyeCRC(d) {
		tsMsg = "ERROR CRC"
		errCls = "err"
	}
	res = strings.ReplaceAll(res, "{D_TS_MSG}", tsMsg)
	res = strings.ReplaceAll(res, "{D_ERR_CLS}", errCls)
	
	totalSolar := d.PV1Power + d.PV2Power + d.PV3Power + d.PV4Power
	res = strings.ReplaceAll(res, "{D_PV}", fmt.Sprintf("%.0f", totalSolar))
	res = strings.ReplaceAll(res, "{D_GEN}", fmt.Sprintf("%.0f", d.GenInv))
	res = strings.ReplaceAll(res, "{D_GRID}", fmt.Sprintf("%.0f", d.Grid))      // Grid R172
	res = strings.ReplaceAll(res, "{D_GCT}", fmt.Sprintf("%.0f", d.GridCTInt))  // Grid CT R169
	res = strings.ReplaceAll(res, "{D_IOUT}", fmt.Sprintf("%.0f", d.InvOutPower)) // Inverter Out R175
	res = strings.ReplaceAll(res, "{D_LOAD}", fmt.Sprintf("%.0f", d.LoadTotal))

	// Batería (Color según carga/descarga)
	batVal := d.BattPower * -1
	batCol := "#3fb950" 
	if batVal < 0 { batCol = "#f85149" }
	res = strings.ReplaceAll(res, "{D_BAT}", fmt.Sprintf("%.0f", batVal))
	res = strings.ReplaceAll(res, "{D_BAT_COL}", batCol)

	res = strings.ReplaceAll(res, "{D_SOC}", fmt.Sprintf("%.0f", d.SOC))
	res = strings.ReplaceAll(res, "{D_TDC}", fmt.Sprintf("%.1f", d.TempDisipDC))
	res = strings.ReplaceAll(res, "{D_TAC}", fmt.Sprintf("%.1f", d.TempDisipAC))
	res = strings.ReplaceAll(res, "{D_TBAT}", fmt.Sprintf("%.1f", d.TempBatt))

	// --- SECCIÓN ETEK ---
	etekTS := time.Unix(e.Timestamp, 0).Format("15:04:05")
	etekStat := "Inactivo"
	if e.NumControllers > 0 {
		switch e.Controllers[0].WorkingStatus {
		case 1: etekStat = "Desconectado"
		case 3: etekStat = "Enchufado"
		case 5: etekStat = "Cargando"
		case 19: etekStat = "Parado"
		default: etekStat = fmt.Sprintf("Status %d", e.Controllers[0].WorkingStatus)
		}
	}
	if !validateEtekCRC(e) && e.Timestamp != 0 { etekStat = "ERROR CRC" }
	if time.Since(time.Unix(e.Timestamp, 0)) > 10*time.Second { etekStat = "OFFLINE" }

	etekCtrls := ""
	for i := 0; i < int(e.NumControllers) && i < 4; i++ {
		c := e.Controllers[i]

		btnLabel := ""
		btnAction := ""
		btnClass := ""
		if c.WorkingStatus == 3 || c.WorkingStatus == 19 {
			btnLabel = "Cargar"
			btnAction = "start"
		} else if c.WorkingStatus == 5 {
			btnLabel = "Stop"
			btnAction = "stop"
			btnClass = "btn-stop"
		}
		
		btnHtml := "" 
		if btnLabel != "" {
			btnHtml = fmt.Sprintf(`<button class="%s" style="padding:2px 8px; font-size:0.8em; height:22px; cursor:pointer; min-width:60px;" onclick="fetch('/evse/%s?id=%d')">%s</button>`, btnClass, btnAction, c.ModbusID, btnLabel)
		}

		line := fmt.Sprintf(`<div class="row" style="border-top:1px solid #21262d; padding:5px 0; align-items:center;">
			<span class="lab" style="flex:1;">ID %d | ST:%d</span>
			<span style="flex:0.5; text-align:center;">%s</span>
			<span class="val" style="flex:1; text-align:right;">%d | %d | %d</span>
		</div>`, c.ModbusID, c.WorkingStatus, btnHtml, c.MaxChargeCurrent, c.RotarySwitchPWM, c.OutputPWMDuty)
		etekCtrls += line
	}
	if e.Timestamp == 0 { etekTS = "--:--:--" }

	res = strings.ReplaceAll(res, "{E_TS}", etekTS)
	res = strings.ReplaceAll(res, "{E_STAT}", etekStat)
	res = strings.ReplaceAll(res, "{E_MARG}", fmt.Sprintf("%.0f", e.MarginW))
	res = strings.ReplaceAll(res, "{E_LIM}", fmt.Sprintf("%.0f", e.LimitW))
	res = strings.ReplaceAll(res, "{E_PINT}", fmt.Sprintf("%.0f", e.IntegratedPower))
	res = strings.ReplaceAll(res, "{E_CTRLS}", etekCtrls)

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, res)
}

func readDeyeSHM() ShmPayload {
	var d ShmPayload
	if addrDeye == 0 { addrDeye = attachSHM(SHM_DEYE, SIZE_DEYE) }
	if addrDeye == 0 { return d }
	src := unsafe.Slice((*byte)(unsafe.Pointer(addrDeye)), SIZE_DEYE)
	_ = binary.Read(bytes.NewReader(src), binary.LittleEndian, &d)
	return d
}

func readMeterSHM() MeterPayload {
	var m MeterPayload
	if addrMeter == 0 { addrMeter = attachSHM(SHM_METER, SIZE_METER) }
	if addrMeter == 0 { return m }
	src := unsafe.Slice((*byte)(unsafe.Pointer(addrMeter)), SIZE_METER)
	_ = binary.Read(bytes.NewReader(src), binary.LittleEndian, &m)
	return m
}

func readEtekSHM() EtekPayload {
	var e EtekPayload
	if addrEtek == 0 { addrEtek = attachSHM(SHM_ETEK, SIZE_ETEK) }
	if addrEtek == 0 { return e }
	size := int(unsafe.Sizeof(e))
	src := unsafe.Slice((*byte)(unsafe.Pointer(addrEtek)), size)
	_ = binary.Read(bytes.NewReader(src), binary.LittleEndian, &e)
	return e
}

func validateDeyeCRC(d ShmPayload) bool {
	orig := d.CRC; d.CRC = 0
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, d)
	return crc32.ChecksumIEEE(buf.Bytes()[:508]) == orig
}

func validateEtekCRC(e EtekPayload) bool {
	orig := e.Crc32; e.Crc32 = 0
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, e)
	return crc32.ChecksumIEEE(buf.Bytes()[:buf.Len()-4]) == orig
}