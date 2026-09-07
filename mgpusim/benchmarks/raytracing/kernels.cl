// OpenCL ray tracer kernel, adapted from
// https://github.com/alexgeek/gpu-raytracer (kernels/trace.cl).
//
// Change vs upstream: the upstream entry point wrote to a
// __write_only image2d_t (OpenGL texture). MGPUSim does not implement
// image (MIMG) instructions, so the output is a plain __global float
// framebuffer instead: 4 floats (RGBA) per pixel, row-major.

typedef struct {
    float4 origin;
    float4 dir;
    float4 col;
} Ray;

typedef struct {
    float4 diffuse_col;
    float diffuse;
    float4 specular_col;
    float specular;
    float reflect;
    float4 pos;
    float4 normal;
    float4 scale;
} Primitive;

#define PRIM_TYPE(P) (int)((P).scale.w)
#define RADIUS(P) P.scale.x
#define SCALE(P) (float3)(P.scale.x, P.scale.y, P.scale.z)
#define PRIM_PLANE 1
#define PRIM_SPHERE 2
#define DELTA  0.001
#define HIT 1
#define MISS 0
#define NONE -1

int ray_plane(Ray* ray, Primitive* prim, float* t) {
    const float dp = dot(ray->dir, prim->normal);
    if(dp == 0) return MISS;
    const float d = dot(prim->normal, prim->pos - ray->origin) / dp;
    if(d > 0 && d < *t) {
        *t = d;
        return HIT;
    }
    return MISS;
}

int ray_sphere(Ray* ray, Primitive* prim, float* t) {
    const float radius = prim->scale.x;
    const float4 v = prim->pos - ray->origin;
    const float dp = dot(ray->dir, v);
    const float det = dp*dp - dot(v, v) + radius*radius;
    if(det <= 0) return MISS;

    float d = (dp - sqrt(det));
    if (d < 0 ) {
        d = (dp + sqrt(det));
        if(d < 0) return MISS;
    }
    *t = d;
    return HIT;
}

int shade(Ray* ray, Primitive* prim, float4 intersection) {
        ray->col += (float4)(0.1f, 0.1f, 0.1f, 1.0f);

        const float4 light_pos = (float4)(-3.0f, 4.0f, -1.0f, 0);
        const float4 light_col = (float4)(0, 0, 0.8f, 1.0f);

        const float4 light_dir = light_pos - intersection;

        const int prim_type = (int)prim->scale.w;
        if(prim_type == PRIM_SPHERE)
        {
            float radius = prim->scale.x;
            prim->normal = (intersection - prim->pos) * radius;
        }

        prim->normal = normalize(prim->normal);

        const float lambertian = max(dot(prim->normal, fast_normalize(light_dir)), 0.0f);

        ray->col += prim->diffuse * lambertian * prim->diffuse_col;

        const float4 bisec = fast_normalize(light_dir + ray->dir);
        const float alpha = 16.0f;
        const float dp2 = pow( max(dot(bisec, prim->normal), 0.0f), alpha);
        ray->col += prim->diffuse * dp2 * prim->specular_col;
}

int ray_trace(Ray* ray, Primitive* prims) {
    const int num_prims = 10;
    int hit = NONE;
    float t = MAXFLOAT;

    for(int p = 0; p < num_prims; p++)
    {
        int prim_type = (int)(prims[p].scale.w);
        switch(prim_type)
        {
            case PRIM_PLANE:
                if(ray_plane(ray, &prims[p], &t)) {
                    hit = p;
                }
                break;
            case PRIM_SPHERE:
                if(ray_sphere(ray, &prims[p], &t)) hit = p;
                break;
        }
    }

    if (hit == NONE) return 0;

    const float4 intersection = ray->origin + t * ray->dir;
    shade(ray, &prims[hit], intersection);
    return 0;
}

inline float calc_uv(float* u, float *v, unsigned int x, unsigned int y, unsigned int width, unsigned int height) {
    const float ratio = (float)width / height;
    *u = ((x+0.5) - (width/2)) / (2 * width) * ratio;
    *v = ((y+0.5) - (height/2)) / (2 * height);
    return ratio;
}

inline Ray calc_ray(float focal, float4 uv, float4 col) {
    Ray ray;
    ray.origin = (float4)(0, 0, -focal , 0);
    ray.dir = fast_normalize(uv - ray.origin);
    ray.col = col;
    return ray;
}

__kernel void pixel_kernel(__global float* fb, unsigned int width, unsigned int height, float time)
{
    const unsigned int x = get_global_id(0);
    const unsigned int y = get_global_id(1);

    if (x >= width || y >= height) return;

    float u, v;
    calc_uv(&u, &v, x, y, width, height);

    Primitive prims[10];

    prims[0].pos = (float4)(0,-.1f,0,0);
    prims[0].diffuse_col =  (float4)(206.0f, 206.0f, 205.0f, 255.0f) / 255.0f;
    prims[0].diffuse = 0.6f;
    prims[0].specular_col = (float4)(206.0f, 206.0f, 205.0f, 255.0f) / 255.0f;
    prims[0].specular = 0.2f;
    prims[0].scale = (float4)(1.0f, 1.0f, 1.0f, PRIM_PLANE);
    prims[0].normal = fast_normalize((float4)(0, 20.0f, -0.1f, 0));
    prims[0].reflect = 0;

    prims[1].pos = (float4)(0, 0, 50.0f, 0);
    prims[1].diffuse_col = (float4)(35.0f, 35.0f, 35.0f, 255.0f) / 255.0f;
    prims[1].diffuse = 0.8f;
    prims[1].specular_col = (float4)(30.0f, 30.0f, 30.0f, 255.0f) / 255.0f;
    prims[1].specular = 0.2f;
    prims[1].scale = (float4)(1.0f, 1.0f, 1.0f, PRIM_PLANE);
    prims[1].normal = normalize((float4)(0.2f, -0.2f, -0.9f, 0));
    prims[1].reflect = 0;

    prims[2].pos = (float4)(2.5f-time, 2.5f, 100.0f, 0);
    prims[2].diffuse_col =  (float4)(255.0f, 154.0f, 12.0f, 255.0f) / 255.0f;
    prims[2].diffuse = 0.7f;
    prims[2].specular_col = (float4)(24.0f, 185.0f, 209.0f, 255.0f) / 255.0f;
    prims[2].specular = 0.95f;
    prims[2].scale = (float4)(1.0f, 1.0f, 1.0f, PRIM_SPHERE);
    prims[2].normal = normalize((float4)(0, 0.1f, 1.0f, 0));
    prims[2].reflect = 0.2f;

    prims[3].pos = (float4)(5.0f*cos(time*10.0f), 1.0f, 50.0f+10.0f*sin(time*10.0f), 0);
    prims[3].diffuse_col = (float4)(250.0f, 115.0f, 57.0f, 255.0f) / 255.0f;
    prims[3].diffuse = 0.7f;
    prims[3].specular_col = (float4)(255.0f, 185.0f, 209.0f, 255.0f) / 255.0f;
    prims[3].specular = 0.9f;
    prims[3].scale = (float4)(2.0f, 1.0f, 1.0f, PRIM_SPHERE);
    prims[3].normal = normalize((float4)(0, 0.1f, 1.0f, 0));
    prims[3].reflect = 0.5f;

    int i = 4;
    for(;i<10;i++) {
        prims[i].pos = (float4)(-1.5f*i+8.0f, .5f, -2.5f*i+60.0f, 0);
        prims[i].diffuse_col = (float4)(24.0f, 200.0f, 213.0f, 255.0f) / 255.0f;
        prims[i].diffuse = 0.6f;
        prims[i].specular_col = (float4)(24.0f, 190.0f, 210.0f, 255.0f) / 255.0f;
        prims[i].specular = 1.0f;
        prims[i].scale = (float4)(1.0f, 1.0f, 1.0f, PRIM_SPHERE);
        prims[i].normal = normalize((float4)(0, 0.1f, 1.0f, 0));
        prims[i].reflect = 1.0f;
    }

    float4 col = (float4)(0,0,0,1.0f);
    for(int i = -1; i < 1; i++) {
        for(int j = -1; j < 1; j++) {
            Ray ray = calc_ray(0.95f, (float4)(u+i*DELTA,v+j*DELTA,0,0), (float4)(0, 0, 0, 1.0f));
            ray_trace(&ray, prims);
            col += ray.col / 9.0f;
        }
    }

    col = clamp(col, 0.0f, 1.0f);

    const unsigned int idx = (y * width + x) * 4;
    fb[idx + 0] = col.x;
    fb[idx + 1] = col.y;
    fb[idx + 2] = col.z;
    fb[idx + 3] = col.w;
}
