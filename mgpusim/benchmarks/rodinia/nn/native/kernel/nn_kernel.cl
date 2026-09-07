/*
 * Nearest-Neighbor distance kernel (Rodinia OpenCL `nn`).
 *
 * Per-record Euclidean distance from a fixed target (lat, lng).
 * One work-item per record; result written to dDistances[globalId].
 *
 * Notes for mgpusim:
 *   - The host marshals LatLong as two consecutive float32 fields, so the
 *     OpenCL struct is laid out as {float lat; float lng;} with 8B size.
 *   - get_global_id(0) is used; numRecords bound check guards over-spawn.
 */

typedef struct latLong {
    float lat;
    float lng;
} LatLong;

__kernel void NearestNeighbor(
    __global const LatLong *dLocations,
    __global       float   *dDistances,
    const int   numRecords,
    const float lat,
    const float lng)
{
    int gid = get_global_id(0);
    if (gid < numRecords) {
        float dlat = lat - dLocations[gid].lat;
        float dlng = lng - dLocations[gid].lng;
        dDistances[gid] = sqrt(dlat * dlat + dlng * dlng);
    }
}
